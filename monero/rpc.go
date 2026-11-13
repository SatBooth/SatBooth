package monero

import (
	"context"
	"errors"
	"fmt"

	"github.com/satbooth/satbooth/xmrrpc"
)

// RPC is the typed calls of xmrrpc that the Monero rail uses: the whole method set of the client (get_version,
// get_height, get_address, create_address, store, get_transfers, get_transfer_by_txid, and sign_transfer as the
// view-only probe). *xmrrpc.Client satisfies it, and so does monerotest.Wallet.
type RPC interface {
	GetVersion(ctx context.Context) (uint32, error)
	GetHeight(ctx context.Context) (int64, error)
	GetAddress(ctx context.Context, account uint32, indices []uint32) (xmrrpc.Addresses, error)
	CreateAddress(ctx context.Context, account uint32, label string) (xmrrpc.Created, error)
	Store(ctx context.Context) error
	GetTransfers(ctx context.Context, q xmrrpc.TransfersQuery) (xmrrpc.Transfers, error)
	GetTransferByTxID(ctx context.Context, txid string, account uint32) ([]xmrrpc.Transfer, error)
	SignTransferProbe(ctx context.Context) (int, error)
}

var _ RPC = (*xmrrpc.Client)(nil)

// wallet-rpc's error codes that the rail reads (src/wallet/wallet_rpc_server_error_codes.h, tag v0.18.5.4).
const (
	// codeNotOpen is WALLET_RPC_ERROR_CODE_NOT_OPEN (:46), what not_open answers to a wallet call when no wallet is
	// open (wallet_rpc_server.cpp:499-503).
	codeNotOpen = -13
	// codeDenied is WALLET_RPC_ERROR_CODE_DENIED (:40), what on_sign_transfer answers first when wallet-rpc runs with
	// --restricted-rpc (wallet_rpc_server.cpp:1369-1374), before any other check.
	codeDenied = -7
	// codeAccountRange is WALLET_RPC_ERROR_CODE_ACCOUNT_INDEX_OUT_OF_BOUNDS (:47): on_getaddress throws
	// account_index_outofbound for an account the wallet does not have (wallet_rpc_server.cpp:681, mapped at
	// :3855-3857).
	codeAccountRange = -14
	// codeIndexRange is WALLET_RPC_ERROR_CODE_ADDRESS_INDEX_OUT_OF_BOUNDS (:48): on_getaddress throws
	// address_index_outofbound for a minor index at or above the account's subaddress count
	// (wallet_rpc_server.cpp:697), which handle_rpc_exception maps to this code (:3860-3862). An account out of range
	// is -14 instead (:681, :3855-3857), which the rail never reads as a forgotten subaddress.
	codeIndexRange = -15
	// codeWatchOnly is WALLET_RPC_ERROR_CODE_WATCH_ONLY (:62): on_sign_transfer answers it for a watch-only wallet
	// before it parses the set (wallet_rpc_server.cpp:1382-1387), after the restricted-mode (DENIED) and no-wallet
	// (-13) answers and the hardware-wallet one (-1, :1375-1380).
	codeWatchOnly = -29
	// labelSatBooth is the label of every subaddress SatBooth creates.
	labelSatBooth = "satbooth"
)

// call bounds one RPC call by Config.CallTimeout, whatever the client's own timeout.
func (s *Service) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.callTimeout)
}

// down reports whether err means wallet-rpc did not answer: a transport failure or a call that ran out of time.
func down(err error) bool {
	return errors.Is(err, xmrrpc.ErrTransport) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// rpcCode is the JSON-RPC error code of err, and whether err is one.
func rpcCode(err error) (int, bool) {
	var e *xmrrpc.Error
	if errors.As(err, &e) {
		return e.Code, true
	}
	return 0, false
}

// unavailable wraps err as ErrUnavailable. xmrrpc's errors carry the method and a code only, never wallet-rpc's text.
func unavailable(err error) error { return fmt.Errorf("%w: %w", ErrUnavailable, err) }
