package onchain_test

import (
	"context"
	"errors"
	"time"

	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/status"
	"github.com/satbooth/satbooth/store"
)

// lnStatus gives the test's Lightning stub the status.Lightning shape; the resolver only calls Status.
type lnStatus struct{ onchain.Lightning }

func (lnStatus) Pay(context.Context, string) (store.Invoice, error) {
	return store.Invoice{}, errors.New("not used")
}
func (lnStatus) ValidateAmount(int64) error { return nil }

// status imports onchain, so a test of package onchain cannot build a status.Resolver itself: this external file
// gives it one through onchain.StatusOf.
func init() {
	onchain.StatusOf = func(st *store.Store, svc *onchain.Service, ln onchain.Lightning, now func() time.Time, o store.Order) (string, error) {
		r := &status.Resolver{St: st, Ln: lnStatus{ln}, Oc: svc, Now: now}
		so, err := r.ByID(context.Background(), o.ID)
		return so.State, err
	}
}
