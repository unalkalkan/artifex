// Package upload serves upload addresses (design decision 23): short-lived
// signed addresses where an agent sends files from its disk, so that their
// content never passes through the conversation.
//
//	PUT /u/<expiry>.<account>.<token>.<signature>/<project>/<path>    one file
//	PUT /u/<expiry>.<account>.<token>.<signature>/<project>/-/archive a tar, gzipped or not
//
// An upload only stores blobs in the project's repository; nothing refers to
// them until a publish names them by the ids the answer gives. The address
// acts as the account and token it was made for, checked again on every
// request, so revoking the token or the role stops it at once.
package upload

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/service"
)

// archivePath is the address's path for an archive. Project paths can't
// start with the folder "-", so it can't be a file's path.
const archivePath = "-/archive"

type Server struct {
	svc *service.Service
	key []byte
	log *zap.Logger
}

func New(svc *service.Service, key []byte, log *zap.Logger) *Server {
	return &Server{svc: svc, key: key, log: log}
}

// Address is what create_upload answers.
type Address struct {
	Project         string    `json:"project"`
	ProjectCreated  bool      `json:"project_created,omitempty"`
	URL             string    `json:"url"`
	ArchiveURL      string    `json:"archive_url"`
	ExpiresAt       time.Time `json:"expires_at"`
	MaxFileBytes    int64     `json:"max_file_bytes"`
	MaxArchiveBytes int64     `json:"max_archive_bytes"`
	Examples        []string  `json:"examples"`
}

// Create makes an upload address for the actor in a project. The caller
// checks the actor's role first.
func (s *Server) Create(a service.Actor, project string) Address {
	cfg := s.svc.Config()
	exp := time.Now().Add(cfg.Limits.UploadURLTTL).Truncate(time.Second)
	base := cfg.BaseURL + "/u/" + s.token(exp.Unix(), a.Account.ID, a.TokenID, project) + "/" + project + "/"
	return Address{
		Project:         project,
		URL:             base,
		ArchiveURL:      base + archivePath,
		ExpiresAt:       exp.UTC(),
		MaxFileBytes:    cfg.Limits.MaxFileBytes,
		MaxArchiveBytes: cfg.Limits.MaxPublishBytes,
		Examples: []string{
			"curl -sS -T screens/home.html '" + base + "screens/home.html'",
			"tar -C ./my-site -czf - . | curl -sS -T - '" + base + archivePath + "'",
		},
	}
}

func (s *Server) token(exp, account, tok int64, project string) string {
	e, ac, t := strconv.FormatInt(exp, 36), strconv.FormatInt(account, 36), strconv.FormatInt(tok, 36)
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("upload\x00" + e + "\x00" + ac + "\x00" + t + "\x00" + project))
	return e + "." + ac + "." + t + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:18])
}

// verify checks a token for a project and returns its account and token ids.
func (s *Server) verify(tok, project string) (int64, int64, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 4 {
		return 0, 0, errors.New("malformed")
	}
	exp, err1 := strconv.ParseInt(parts[0], 36, 64)
	account, err2 := strconv.ParseInt(parts[1], 36, 64)
	t, err3 := strconv.ParseInt(parts[2], 36, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, errors.New("malformed")
	}
	if !hmac.Equal([]byte(tok), []byte(s.token(exp, account, t, project))) {
		return 0, 0, errors.New("bad signature")
	}
	if time.Now().Unix() > exp {
		return 0, 0, errors.New("expired")
	}
	return account, t, nil
}

// ServeHTTP stores what is sent to /u/<token>/<project>/<path>.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		w.Header().Set("Allow", "PUT, POST")
		s.answer(w, http.StatusMethodNotAllowed, errorBody("send files with PUT, such as curl -T <file> <address><path>"))
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/u/"), "/", 3)
	if len(parts) < 2 {
		s.answer(w, http.StatusNotFound, errorBody("not an upload address"))
		return
	}
	tok, project, p := parts[0], parts[1], ""
	if len(parts) == 3 {
		p = parts[2]
	}
	account, tokenID, err := s.verify(tok, project)
	if err != nil {
		s.answer(w, http.StatusForbidden, errorBody("this upload address has expired or is not valid; ask create_upload for a new one"))
		return
	}
	ctx := r.Context()
	a, err := s.svc.ActorByID(ctx, account, tokenID)
	if err != nil {
		s.fail(w, err)
		return
	}
	lim := s.svc.Config().Limits
	var res service.UploadResult
	if p == archivePath {
		r.Body = http.MaxBytesReader(w, r.Body, lim.MaxRequestBytes())
		res, err = s.svc.StoreArchive(ctx, a, project, r.Body)
	} else {
		if p == "" || strings.HasSuffix(p, "/") {
			s.answer(w, http.StatusBadRequest, errorBody("add the file's path in the project to the address, such as <address>assets/app.css"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, lim.MaxFileBytes+1)
		res, err = s.svc.StoreUpload(ctx, a, project, p, r.Body)
	}
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.answer(w, http.StatusRequestEntityTooLarge, errorBody("the request is larger than an upload may be"))
			return
		}
		s.fail(w, err)
		return
	}
	s.log.Info("uploaded", zap.String("project", project), zap.String("by", a.Name()), zap.Int("files", len(res.Changes)), zap.Int64("bytes", res.Bytes))
	s.answer(w, http.StatusCreated, res)
}

func errorBody(msg string) map[string]string { return map[string]string{"error": msg} }

func (s *Server) fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	msg := "the upload failed; the server log has the details"
	switch service.KindOf(err) {
	case service.KindInvalid:
		status, msg = http.StatusBadRequest, err.Error()
		if strings.Contains(msg, "limit") || strings.Contains(msg, "larger than") {
			status = http.StatusRequestEntityTooLarge
		}
	case service.KindNotFound:
		status, msg = http.StatusNotFound, err.Error()
	case service.KindForbidden:
		status, msg = http.StatusForbidden, err.Error()
	case service.KindConflict:
		status, msg = http.StatusConflict, err.Error()
	default:
		s.log.Error("upload failed", zap.Error(err))
	}
	s.answer(w, status, errorBody(msg))
}

func (s *Server) answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	enc.Encode(v)
}
