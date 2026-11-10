package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/store"
)

// SignIn finds or creates the account for a verified email (Google sign-in
// or development sign-in). New accounts wait for an Admin, except emails in
// the configured Admins list, and except with development sign-in, where
// autoActivate is true.
func (s *Service) SignIn(ctx context.Context, email, name string, autoActivate bool) (store.Account, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	acct, err := s.store.AccountByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		role, status := store.RoleMember, store.StatusPending
		if s.cfg.IsAdminEmail(email) {
			role, status = store.RoleAdmin, store.StatusActive
		} else if autoActivate {
			status = store.StatusActive
		}
		acct, err = s.store.CreateAccount(ctx, email, name, role, status)
		if err != nil {
			return acct, err
		}
		s.log.Info("account created", zap.String("email", email), zap.String("status", status))
	} else if err != nil {
		return acct, err
	} else {
		if acct.Name == "" && name != "" {
			s.store.SetAccountName(ctx, acct.ID, name)
			acct.Name = name
		}
		// A configured Admin who was pending (or a member) is promoted.
		if s.cfg.IsAdminEmail(email) && acct.Status != store.StatusDisabled && (!acct.IsAdmin() || !acct.IsActive()) {
			s.store.SetAccountRole(ctx, acct.ID, store.RoleAdmin)
			s.store.SetAccountStatus(ctx, acct.ID, store.StatusActive)
			acct, _ = s.store.AccountByID(ctx, acct.ID)
		}
	}
	if acct.IsActive() {
		s.store.ApplyInvites(ctx, acct)
		s.store.TouchAccount(ctx, acct.ID)
	}
	return acct, nil
}

// AddAccount creates an active account from the server's command line.
func (s *Service) AddAccount(ctx context.Context, email, name string, admin bool) (store.Account, error) {
	email, err := cleanEmail(email)
	if err != nil {
		return store.Account{}, err
	}
	role := store.RoleMember
	if admin {
		role = store.RoleAdmin
	}
	acct, err := s.store.AccountByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		acct, err = s.store.CreateAccount(ctx, email, name, role, store.StatusActive)
	} else if err == nil {
		s.store.SetAccountRole(ctx, acct.ID, role)
		s.store.SetAccountStatus(ctx, acct.ID, store.StatusActive)
		acct, err = s.store.AccountByID(ctx, acct.ID)
	}
	if err == nil {
		s.store.ApplyInvites(ctx, acct)
	}
	return acct, err
}

// ListAccounts lists every account. Admins only.
func (s *Service) ListAccounts(ctx context.Context, a Actor) ([]store.Account, error) {
	if !a.Account.IsAdmin() {
		return nil, forbidden("only Admins manage accounts")
	}
	return s.store.ListAccounts(ctx)
}

// AccountAction is one Admin action on an account.
type AccountAction string

const (
	ActionVerify     AccountAction = "verify"
	ActionDisable    AccountAction = "disable"
	ActionEnable     AccountAction = "enable"
	ActionRemove     AccountAction = "remove"
	ActionMakeAdmin  AccountAction = "make-admin"
	ActionMakeMember AccountAction = "make-member"
)

// ManageAccount applies an Admin action. Admins never act on their own
// account (design decision 15).
func (s *Service) ManageAccount(ctx context.Context, a Actor, id int64, action AccountAction) error {
	if !a.Account.IsAdmin() {
		return forbidden("only Admins manage accounts")
	}
	if id == a.Account.ID {
		return forbidden("Admins can't change their own account")
	}
	acct, err := s.store.AccountByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return notFound("account not found")
	} else if err != nil {
		return err
	}
	switch action {
	case ActionVerify, ActionEnable:
		if err := s.store.SetAccountStatus(ctx, id, store.StatusActive); err != nil {
			return err
		}
		acct.Status = store.StatusActive
		return s.store.ApplyInvites(ctx, acct)
	case ActionDisable:
		if err := s.store.SetAccountStatus(ctx, id, store.StatusDisabled); err != nil {
			return err
		}
		return s.store.DeleteSessionsFor(ctx, id)
	case ActionRemove:
		owned, err := s.store.SoleOwnerOf(ctx, id)
		if err != nil {
			return err
		}
		if len(owned) > 0 {
			return conflict("%s is the only owner of %s; give those projects another owner first", acct.Email, strings.Join(owned, ", "))
		}
		return s.store.DeleteAccount(ctx, id)
	case ActionMakeAdmin:
		return s.store.SetAccountRole(ctx, id, store.RoleAdmin)
	case ActionMakeMember:
		return s.store.SetAccountRole(ctx, id, store.RoleMember)
	}
	return invalid("unknown action %q", action)
}

// NewToken is a token as created: the secret is shown once.
type NewToken struct {
	store.Token
	Secret string
}

// CreateToken makes a token for the actor's own account (design decision 17).
func (s *Service) CreateToken(ctx context.Context, account store.Account, label string) (NewToken, error) {
	label = strings.Join(strings.Fields(label), " ")
	if label == "" {
		label = "Claude"
	}
	if len(label) > 60 {
		return NewToken{}, invalid("the label is longer than 60 characters")
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return NewToken{}, err
	}
	secret := "afx_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
	t, err := s.store.CreateToken(ctx, account.ID, label, HashSecret(secret), secret[:12])
	if err != nil {
		return NewToken{}, err
	}
	return NewToken{Token: t, Secret: secret}, nil
}

// ListTokens lists the actor's tokens; Admins can list everyone's with all.
func (s *Service) ListTokens(ctx context.Context, a Actor, all bool) ([]store.Token, error) {
	if all {
		if !a.Account.IsAdmin() {
			return nil, forbidden("only Admins see everyone's tokens")
		}
		return s.store.ListTokens(ctx, 0)
	}
	return s.store.ListTokens(ctx, a.Account.ID)
}

// RevokeToken revokes one of the actor's tokens, or any token for an Admin.
func (s *Service) RevokeToken(ctx context.Context, a Actor, id int64) error {
	t, err := s.store.TokenByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return notFound("token not found")
	} else if err != nil {
		return err
	}
	if t.AccountID != a.Account.ID && !a.Account.IsAdmin() {
		return notFound("token not found")
	}
	return s.store.RevokeToken(ctx, id)
}

// Authenticate turns a token secret into an Actor. The account must be active.
func (s *Service) Authenticate(ctx context.Context, secret string) (Actor, error) {
	secret = strings.TrimSpace(secret)
	if !strings.HasPrefix(secret, "afx_") {
		return Actor{}, forbidden("invalid token")
	}
	t, acct, err := s.store.TokenByHash(ctx, HashSecret(secret))
	if err != nil {
		return Actor{}, forbidden("invalid token")
	}
	if !acct.IsActive() {
		return Actor{}, forbidden("the token's account is not active")
	}
	if t.LastUsedAt == nil || time.Since(*t.LastUsedAt) > time.Minute {
		s.store.TouchToken(ctx, t.ID)
	}
	return Actor{Account: acct, AgentLabel: t.Label, TokenID: t.ID}, nil
}

// HashSecret is how tokens and session ids are stored.
func HashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}
