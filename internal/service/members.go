package service

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/unalkalkan/artifex/internal/store"
)

// MemberView is a project member or a waiting invitation.
type MemberView struct {
	Email   string    `json:"email"`
	Name    string    `json:"name,omitempty"`
	Role    string    `json:"role"`
	Invited bool      `json:"invited,omitempty"` // no account yet, or not verified yet
	Since   time.Time `json:"since"`
}

// ListMembers lists a project's members and invitations.
func (s *Service) ListMembers(ctx context.Context, a Actor, project string) ([]MemberView, error) {
	if _, err := s.Require(ctx, a, project, store.ProjectCommenter); err != nil {
		return nil, err
	}
	ms, err := s.store.ListMembers(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]MemberView, 0, len(ms))
	for _, m := range ms {
		out = append(out, MemberView{Email: m.Account.Email, Name: m.Account.Name, Role: m.Role, Since: m.AddedAt, Invited: !m.Account.IsActive()})
	}
	ins, err := s.store.ListInvites(ctx, project)
	if err != nil {
		return nil, err
	}
	for _, in := range ins {
		out = append(out, MemberView{Email: in.Email, Role: in.Role, Invited: true, Since: in.CreatedAt})
	}
	return out, nil
}

// SetMember gives an email a role in the project. An email with no account
// yet is kept as an invitation and applied when its account is verified.
// Owners only.
func (s *Service) SetMember(ctx context.Context, a Actor, project, email, role string) (MemberView, error) {
	if _, err := s.Require(ctx, a, project, store.ProjectOwner); err != nil {
		return MemberView{}, err
	}
	role = strings.ToLower(strings.TrimSpace(role))
	if !store.ValidProjectRole(role) {
		return MemberView{}, invalid("role must be commenter, editor or owner")
	}
	email, err := cleanEmail(email)
	if err != nil {
		return MemberView{}, err
	}
	acct, err := s.store.AccountByEmail(ctx, email)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if err := s.store.SetInvite(ctx, project, email, role, a.Account.ID); err != nil {
			return MemberView{}, err
		}
		return MemberView{Email: email, Role: role, Invited: true, Since: s.now()}, nil
	case err != nil:
		return MemberView{}, err
	}
	if err := s.store.SetMember(ctx, project, acct.ID, role); err != nil {
		if errors.Is(err, store.ErrLastOwner) {
			return MemberView{}, conflict("%s is the project's only owner; make someone else an owner first", email)
		}
		return MemberView{}, err
	}
	return MemberView{Email: acct.Email, Name: acct.Name, Role: role, Invited: !acct.IsActive(), Since: s.now()}, nil
}

// RemoveMember removes a member or an invitation. Owners only; anyone may
// leave a project themselves, unless they are its only owner.
func (s *Service) RemoveMember(ctx context.Context, a Actor, project, email string) error {
	email, err := cleanEmail(email)
	if err != nil {
		return err
	}
	min := store.ProjectOwner
	if strings.EqualFold(email, a.Account.Email) {
		min = store.ProjectCommenter
	}
	if _, err := s.Require(ctx, a, project, min); err != nil {
		return err
	}
	acct, err := s.store.AccountByEmail(ctx, email)
	if err == nil {
		err = s.store.RemoveMember(ctx, project, acct.ID)
		if err == nil {
			return nil
		}
		if errors.Is(err, store.ErrLastOwner) {
			return conflict("%s is the project's only owner; make someone else an owner first", email)
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err := s.store.DeleteInvite(ctx, project, email); errors.Is(err, store.ErrNotFound) {
		return notFound("%s is not a member of %q", email, project)
	} else if err != nil {
		return err
	}
	return nil
}

func cleanEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || !strings.Contains(email, "@") {
		return "", invalid("%q is not an email address", email)
	}
	return email, nil
}
