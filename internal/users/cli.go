package users

import (
	"context"
	"fmt"
)

// AddUser inserts (or no-ops) a user. Used by `app --add-user EMAIL [--admin]`.
// Prints the resulting user; returns nil on success.
func AddUser(ctx context.Context, repo *Repository, email string, admin bool) error {
	u, err := repo.Upsert(ctx, email, "", admin)
	if err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	fmt.Printf("user: id=%d email=%s is_admin=%t is_active=%t\n", u.ID, u.Email, u.IsAdmin, u.IsActive)
	return nil
}

// DisableUser flips is_active to false. Used by `app --disable-user EMAIL`.
func DisableUser(ctx context.Context, repo *Repository, email string) error {
	if err := repo.SetActive(ctx, email, false); err != nil {
		return fmt.Errorf("disable: %w", err)
	}
	fmt.Printf("disabled: %s\n", email)
	return nil
}
