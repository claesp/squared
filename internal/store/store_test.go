package store_test

import (
	"context"
	"errors"
	"testing"

	"squared/internal/domain"
	"squared/internal/store/storetest"
)

func TestMigrateTwice(t *testing.T) {
	st := storetest.New(t)
	// The second run must do nothing.
	storetest.Must(t, st.Migrate(context.Background(), storetest.Logger()))
}

func TestUniqueEmail(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	storetest.Must(t, st.CreateUser(ctx, &domain.User{Email: "a@example.com", Name: "A", PasswordHash: "x"}))
	err := st.CreateUser(ctx, &domain.User{Email: "a@example.com", Name: "B", PasswordHash: "x"})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestBadUUIDIsNotFound(t *testing.T) {
	st := storetest.New(t)
	if _, err := st.UserByID(context.Background(), "not-a-uuid"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestOutbox(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	for _, id := range []string{"1", "2", "3"} {
		storetest.Must(t, st.EnqueueTask(ctx, domain.Task{Kind: domain.TaskTenantSync, TenantID: id}))
	}

	// The publisher fails on the second task: the first task is marked,
	// the others stay.
	calls := 0
	n, err := st.PublishOutbox(ctx, 10, func(ctx context.Context, payload []byte) error {
		calls++
		if calls == 2 {
			return errors.New("broker is down")
		}
		return nil
	})
	if err == nil || n != 1 {
		t.Fatalf("first run: n=%d err=%v; want n=1 and an error", n, err)
	}

	var got []string
	n, err = st.PublishOutbox(ctx, 10, func(ctx context.Context, payload []byte) error {
		got = append(got, string(payload))
		return nil
	})
	storetest.Must(t, err)
	if n != 2 || len(got) != 2 {
		t.Fatalf("second run published %d tasks, want 2: %v", n, got)
	}
	n, err = st.PublishOutbox(ctx, 10, func(context.Context, []byte) error { return nil })
	storetest.Must(t, err)
	if n != 0 {
		t.Fatalf("third run published %d tasks, want 0", n)
	}
}
