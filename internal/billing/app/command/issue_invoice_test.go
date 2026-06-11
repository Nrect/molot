package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/billing/app/command"
	"molot/internal/billing/domain/invoice"
	"molot/internal/common/errs"
)

func issueCmd(t *testing.T) command.IssueInvoice {
	t.Helper()
	id, err := invoice.NewInvoiceID(uuid.New())
	require.NoError(t, err)
	auctionID, err := invoice.NewAuctionID(uuid.New())
	require.NoError(t, err)
	debtor, err := invoice.NewBidderID(uuid.New())
	require.NoError(t, err)
	return command.IssueInvoice{
		InvoiceID: id,
		AuctionID: auctionID,
		Debtor:    debtor,
		Hammer:    testMoney(t, 100_000),
		Attempt:   invoice.AttemptFirst,
	}
}

func TestIssueInvoice(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	t.Run("issues a pending invoice with snapshotted amounts", func(t *testing.T) {
		t.Parallel()
		repo := &repoSpy{}
		h := command.NewIssueInvoiceHandler(repo, testPolicy(t), testTerm(t), fixedClock{now: now})
		cmd := issueCmd(t)

		require.NoError(t, h.Handle(context.Background(), cmd))

		require.Len(t, repo.added, 1)
		inv := repo.added[0]
		assert.Equal(t, cmd.InvoiceID, inv.ID())
		assert.Equal(t, int64(10_000), inv.Commission().Amount())
		assert.Equal(t, int64(110_000), inv.Total().Amount())
		assert.Equal(t, now.Add(48*time.Hour), inv.DueAt())
		assert.True(t, inv.IsPending())
	})

	t.Run("already issued maps to nil (idempotent redelivery)", func(t *testing.T) {
		t.Parallel()
		repo := &repoSpy{addErr: invoice.ErrInvoiceAlreadyIssued}
		h := command.NewIssueInvoiceHandler(repo, testPolicy(t), testTerm(t), fixedClock{now: now})

		assert.NoError(t, h.Handle(context.Background(), issueCmd(t)))
		assert.Len(t, repo.added, 1, "the insert attempt must still happen")
	})

	t.Run("invalid command is incorrect input", func(t *testing.T) {
		t.Parallel()
		repo := &repoSpy{}
		h := command.NewIssueInvoiceHandler(repo, testPolicy(t), testTerm(t), fixedClock{now: now})
		cmd := issueCmd(t)
		cmd.Hammer = invoice.Money{}

		err := h.Handle(context.Background(), cmd)
		assert.Equal(t, errs.ErrorKindIncorrectInput, errs.KindFromError(err))
		assert.Empty(t, repo.added)
	})

	t.Run("infrastructure failure is passed through", func(t *testing.T) {
		t.Parallel()
		infraErr := errors.New("db down")
		repo := &repoSpy{addErr: infraErr}
		h := command.NewIssueInvoiceHandler(repo, testPolicy(t), testTerm(t), fixedClock{now: now})

		assert.ErrorIs(t, h.Handle(context.Background(), issueCmd(t)), infraErr)
	})
}
