package onchain

import (
	"context"
	"errors"
	"fmt"

	"github.com/satbooth/satbooth/btcrpc"
)

// RPC is the typed calls of btcrpc that the on-chain rail uses, and no others (the whole whitelist of the client:
// getnewaddress, getaddressinfo, deriveaddresses, listsinceblock, gettransaction, getmempoolentry,
// getblockchaininfo, getwalletinfo, listdescriptors, getdescriptorinfo, getmempoolinfo, gettxspendingprevout).
// *btcrpc.Client satisfies it, and so does onchaintest.Node.
type RPC interface {
	GetBlockchainInfo(ctx context.Context) (btcrpc.BlockchainInfo, error)
	GetWalletInfo(ctx context.Context) (btcrpc.WalletInfo, error)
	GetDescriptorInfo(ctx context.Context, desc string) (btcrpc.DescriptorInfo, error)
	ListDescriptors(ctx context.Context) (btcrpc.Descriptors, error)
	GetNewAddress(ctx context.Context, addressType string) (string, error)
	GetAddressInfo(ctx context.Context, address string) (btcrpc.AddressInfo, error)
	DeriveAddresses(ctx context.Context, desc string, index int64) ([]string, error)
	ListSinceBlock(ctx context.Context, blockhash string, targetConfirmations int) (btcrpc.SinceBlock, error)
	GetTransaction(ctx context.Context, txid string) (btcrpc.Transaction, error)
	InMempool(ctx context.Context, txid string) (bool, error)
	MempoolLoaded(ctx context.Context) (bool, error)
	SpentInMempool(ctx context.Context, outs []btcrpc.Outpoint) ([]string, error)
}

var _ RPC = (*btcrpc.Client)(nil)

// call bounds one RPC call by Config.CallTimeout, whatever the client's own timeout.
func (s *Service) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.callTimeout)
}

// down reports whether err means bitcoind did not answer: a transport failure or a call that ran out of time.
func down(err error) bool {
	return errors.Is(err, btcrpc.ErrTransport) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// unavailable wraps err as ErrUnavailable. btcrpc's errors carry the method and a code only, never bitcoind's text.
func unavailable(err error) error { return fmt.Errorf("%w: %w", ErrUnavailable, err) }
