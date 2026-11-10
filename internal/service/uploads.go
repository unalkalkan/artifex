package service

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"path"
	"strings"

	"github.com/unalkalkan/artifex/internal/store"
)

// Uploaded is one file an upload address stored (design decision 23), in the
// shape of a publish change: an agent passes it to publish as it is.
type Uploaded struct {
	Path   string `json:"path"`
	Upload string `json:"upload"`
}

// UploadResult is an upload address's answer.
type UploadResult struct {
	Changes []Uploaded `json:"changes"`
	Bytes   int64      `json:"bytes"`
	Skipped []string   `json:"skipped,omitempty"`
}

// maxArchiveFiles is how many files one archive may hold.
const maxArchiveFiles = 10000

// ActorByID rebuilds the actor an upload address was made for. The account
// must still be active and the token, when there was one, not revoked, so an
// address stops working with the access it came from.
func (s *Service) ActorByID(ctx context.Context, accountID, tokenID int64) (Actor, error) {
	acct, err := s.store.AccountByID(ctx, accountID)
	if err != nil || !acct.IsActive() {
		return Actor{}, forbidden("this upload address's account is not active")
	}
	a := Actor{Account: acct}
	if tokenID != 0 {
		t, err := s.store.TokenByID(ctx, tokenID)
		if err != nil || t.AccountID != accountID || t.RevokedAt != nil {
			return Actor{}, forbidden("the token this upload address was made with is revoked")
		}
		a.AgentLabel, a.TokenID = t.Label, t.ID
	}
	return a, nil
}

// errTooLarge is what a limitedReader returns past its limit.
var errTooLarge = errors.New("too large")

// limitedReader fails once more than n bytes come through, instead of
// stopping quietly as io.LimitReader does.
type limitedReader struct {
	r    io.Reader
	n    int64
	read int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.read += int64(n)
	if l.read > l.n {
		return n, errTooLarge
	}
	return n, err
}

// StoreUpload stores one file sent to an upload address as a blob of the
// project. Nothing refers to it until a publish names it; p is where the
// agent means to publish it, checked now so the change it gets back is valid.
func (s *Service) StoreUpload(ctx context.Context, a Actor, project, p string, body io.Reader) (UploadResult, error) {
	if _, err := s.Require(ctx, a, project, store.ProjectEditor); err != nil {
		return UploadResult{}, err
	}
	p, err := CleanPath(p)
	if err != nil {
		return UploadResult{}, err
	}
	max := s.cfg.Limits.MaxFileBytes
	lr := &limitedReader{r: body, n: max}
	id, err := s.Repo(project).WriteBlob(ctx, lr)
	if errors.Is(err, errTooLarge) {
		return UploadResult{}, invalid("%s is larger than the limit of %d bytes", p, max)
	}
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{Changes: []Uploaded{{Path: p, Upload: id}}, Bytes: lr.read}, nil
}

// StoreArchive stores every file of a tar archive, gzipped or not, as blobs
// of the project, at the archive's paths. It refuses the whole archive when
// one file is too large or the files add up to more than one publish, so
// nothing is published half. Folders are skipped silently; links, devices
// and paths a project can't hold are skipped and listed.
func (s *Service) StoreArchive(ctx context.Context, a Actor, project string, body io.Reader) (UploadResult, error) {
	if _, err := s.Require(ctx, a, project, store.ProjectEditor); err != nil {
		return UploadResult{}, err
	}
	br := bufio.NewReader(body)
	var src io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return UploadResult{}, invalid("the archive is not valid gzip: %v", err)
		}
		defer zr.Close()
		src = zr
	}
	lim := s.cfg.Limits
	repo := s.Repo(project)
	tr := tar.NewReader(src)
	var res UploadResult
	at := map[string]int{} // path -> index in res.Changes; a later entry wins
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return UploadResult{}, invalid("the archive is not a valid tar file: %v", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg: // the reader reports old tars' '\x00' as this too
		default:
			if name != "" && name != "." {
				res.Skipped = append(res.Skipped, name+" (not a regular file)")
			}
			continue
		}
		base := path.Base(name)
		if strings.HasPrefix(base, "._") || base == ".DS_Store" {
			res.Skipped = append(res.Skipped, name+" (macOS metadata)")
			continue
		}
		p, err := CleanPath(name)
		if err != nil {
			res.Skipped = append(res.Skipped, name+" ("+err.Error()+")")
			continue
		}
		if h.Size > lim.MaxFileBytes {
			return UploadResult{}, invalid("%s is %d bytes; the limit is %d. Nothing was stored", p, h.Size, lim.MaxFileBytes)
		}
		if res.Bytes+h.Size > lim.MaxPublishBytes {
			return UploadResult{}, invalid("the archive's files add up to more than one publish may hold (%d bytes). Nothing was stored; send it in parts", lim.MaxPublishBytes)
		}
		if len(res.Changes) >= maxArchiveFiles {
			return UploadResult{}, invalid("the archive has more than %d files; send it in parts", maxArchiveFiles)
		}
		id, err := repo.WriteBlob(ctx, tr)
		if err != nil {
			return UploadResult{}, invalid("the archive ended inside %s: %v", p, err)
		}
		res.Bytes += h.Size
		if i, ok := at[p]; ok {
			res.Changes[i].Upload = id
			continue
		}
		at[p] = len(res.Changes)
		res.Changes = append(res.Changes, Uploaded{Path: p, Upload: id})
	}
	if len(res.Changes) == 0 {
		return res, invalid("the archive has no files")
	}
	return res, nil
}
