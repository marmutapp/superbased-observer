package store

import (
	"context"
	"testing"
)

func TestEnrolment_RoundTripAndDelete(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Not enrolled: LoadEnrolment is (nil, nil), never an error.
	got, err := s.LoadEnrolment(ctx)
	if err != nil {
		t.Fatalf("LoadEnrolment (empty): %v", err)
	}
	if got != nil {
		t.Fatalf("LoadEnrolment (empty) = %+v, want nil", got)
	}

	want := Enrolment{
		OrgID: "org-1", OrgName: "Acme", OrgServerURL: "https://org.acme.example",
		UserID: "scim-42", UserEmail: "dev@acme.example",
		EnrolledAt: "2026-05-26T10:00:00Z", BearerKeyID: "sbo-org-bearer-v1",
	}
	if err := s.WriteEnrolment(ctx, want); err != nil {
		t.Fatalf("WriteEnrolment: %v", err)
	}

	got, err = s.LoadEnrolment(ctx)
	if err != nil {
		t.Fatalf("LoadEnrolment: %v", err)
	}
	if got == nil || *got != want {
		t.Fatalf("LoadEnrolment = %+v, want %+v", got, want)
	}

	// Re-write upgrades the singleton in place (id = 1 stays unique).
	want.OrgName = "Acme Corp"
	if err := s.WriteEnrolment(ctx, want); err != nil {
		t.Fatalf("WriteEnrolment (upgrade): %v", err)
	}
	got, _ = s.LoadEnrolment(ctx)
	if got == nil || got.OrgName != "Acme Corp" {
		t.Fatalf("LoadEnrolment after upgrade = %+v, want OrgName=Acme Corp", got)
	}

	// Delete returns to the not-enrolled state; deleting again is a no-op.
	if err := s.DeleteEnrolment(ctx); err != nil {
		t.Fatalf("DeleteEnrolment: %v", err)
	}
	if got, _ := s.LoadEnrolment(ctx); got != nil {
		t.Fatalf("LoadEnrolment after delete = %+v, want nil", got)
	}
	if err := s.DeleteEnrolment(ctx); err != nil {
		t.Fatalf("DeleteEnrolment (idempotent): %v", err)
	}
}

// WriteEnrolment defaults EnrolledAt to now when the caller leaves it empty.
func TestWriteEnrolment_DefaultsEnrolledAt(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if err := s.WriteEnrolment(ctx, Enrolment{
		OrgID: "o", OrgName: "n", OrgServerURL: "u", UserID: "uid", UserEmail: "e", BearerKeyID: "k",
	}); err != nil {
		t.Fatalf("WriteEnrolment: %v", err)
	}
	got, _ := s.LoadEnrolment(ctx)
	if got == nil || got.EnrolledAt == "" {
		t.Fatalf("EnrolledAt was not defaulted: %+v", got)
	}
}
