package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/db"
)

type initialAdmin struct {
	Username string
	Password string
	Created  bool
}

func credential() (string, error) {
	b := make([]byte, 24)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func bootstrapInitialAdmin(ctx context.Context, repo *db.Repository) (initialAdmin, error) {
	if _, e := repo.UserByID(ctx, 1); e == nil {
		return initialAdmin{}, nil
	} else if e != db.ErrNotFound {
		return initialAdmin{}, e
	}
	return createInitial(ctx, repo, true)
}
func resetInitialAdmin(ctx context.Context, repo *db.Repository) (initialAdmin, error) {
	if _, e := repo.UserByID(ctx, 1); e != nil {
		return initialAdmin{}, fmt.Errorf("initial admin: %w", e)
	}
	return createInitial(ctx, repo, false)
}
func createInitial(ctx context.Context, repo *db.Repository, create bool) (initialAdmin, error) {
	name, e := credential()
	if e != nil {
		return initialAdmin{}, e
	}
	pass, e := credential()
	if e != nil {
		return initialAdmin{}, e
	}
	hash, e := auth.HashPassword(pass)
	if e != nil {
		return initialAdmin{}, e
	}
	if create {
		u, e := repo.CreateUser(ctx, db.User{Username: "sfh-" + name, PasswordHash: hash})
		if e != nil {
			return initialAdmin{}, e
		}
		return initialAdmin{Username: u.Username, Password: pass, Created: true}, nil
	}
	username := "sfh-" + name
	if e := repo.ResetInitialAdmin(ctx, username, hash); e != nil {
		return initialAdmin{}, e
	}
	return initialAdmin{Username: username, Password: pass}, nil
}
