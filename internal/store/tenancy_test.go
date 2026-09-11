package store

import (
	"context"
	"testing"
)

func TestLegacyOwnershipMigration(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	legacy, err := s.CreateWorkspace(ctx, "Existing project")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO owners(username,password_hash) VALUES('original','hash');
 ALTER TABLE workspaces DROP COLUMN owner_username;
 ALTER TABLE owners DROP COLUMN role;
 ALTER TABLE owners DROP COLUMN email;
 DELETE FROM schema_version WHERE version>=3;`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var role string
	if err = s.DB.QueryRow(ctx, `SELECT role FROM owners WHERE username='original'`).Scan(&role); err != nil || role != "admin" {
		t.Fatal(role, err)
	}
	owned, err := s.WorkspaceOwned(ctx, legacy.ID, "original")
	if err != nil || !owned {
		t.Fatal("legacy data ownership lost", err)
	}
	if _, err = s.DB.Exec(ctx, `INSERT INTO owners(username,password_hash) VALUES('invited','hash')`); err != nil {
		t.Fatal(err)
	}
	private, err := s.CreateWorkspaceOwned(ctx, "Private", "invited")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT role FROM owners WHERE username='invited'`).Scan(&role); err != nil || role != "user" {
		t.Fatal("restart promoted invited user", role, err)
	}
	owned, err = s.WorkspaceOwned(ctx, private.ID, "invited")
	if err != nil || !owned {
		t.Fatal("restart reassigned workspace", err)
	}
}
