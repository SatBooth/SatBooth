package onchain

import (
	"context"
	"fmt"
	"strings"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/store"
)

// chainName is getblockchaininfo's "chain" for a configured network (src/rpc/blockchain.cpp).
func chainName(network string) string {
	switch network {
	case "mainnet":
		return "main"
	case "testnet":
		return "test"
	}
	return network // signet, regtest
}

// Check runs the startup checks against bitcoind. Each failure is a fixed error. A pass clears the behind flag.
func (s *Service) Check(ctx context.Context) error {
	// 1. the chain
	c, cancel := s.call(ctx)
	bi, err := s.rpc.GetBlockchainInfo(c)
	cancel()
	if err != nil {
		return unavailable(err)
	}
	if bi.Chain != chainName(s.cfg.Network) {
		return ErrNetwork
	}
	if bi.InitialBlockDownload || bi.Headers-bi.Blocks > 3 {
		s.log.Error(logIBD)
	}
	// getmempoolinfo, which every pass calls: a whitelist without it (an upgrade that did not add the 11th
	// method) fails here, at startup, rather than in every pass. A mempool still loading is no failure.
	c, cancel = s.call(ctx)
	_, err = s.rpc.MempoolLoaded(c)
	cancel()
	if err != nil {
		return unavailable(err)
	}
	// gettxspendingprevout, which a pass calls when a payment left the node's mempool: a whitelist without it (an
	// upgrade that did not add the 12th method) would end every pass at the first fee bump, so it fails here. One fixed
	// outpoint that no transaction has (the answer lists it with no spender). The probe is also the version gate: the
	// options object (mempool_only) of gettxspendingprevout exists from Bitcoin Core 31, so an older node refuses it
	// here (-8), and the rule that needs the wallet's mempoolconflicts (Core 28 and later) never runs against one.
	c, cancel = s.call(ctx)
	_, err = s.rpc.SpentInMempool(c, []btcrpc.Outpoint{{TxID: strings.Repeat("0", 64)}})
	cancel()
	if err != nil {
		return unavailable(err)
	}
	// The watcher's first scan starts at this tip, never at the genesis (an answer over 8 MiB is a bad answer). It is
	// recorded before any address can be issued (Address waits for a passed Check), so every payment to an issued
	// address is mined above it. A stored cursor is never moved here.
	if _, ok, err := s.st.Cursor(ctx); err != nil {
		return err
	} else if !ok {
		if _, err := s.st.RecordScan(ctx, nil, store.Cursor{BlockHash: bi.BestBlockHash, Height: bi.Blocks, At: s.now()}); err != nil {
			return err
		}
	}

	// 2. the wallet
	c, cancel = s.call(ctx)
	wi, err := s.rpc.GetWalletInfo(c)
	cancel()
	if err != nil {
		if down(err) {
			return unavailable(err)
		}
		return fmt.Errorf("%w: %w", ErrWallet, err)
	}
	if !wi.Descriptors || wi.PrivateKeysEnabled {
		return ErrWallet
	}

	// 3. the descriptor, as bitcoind normalises it
	c, cancel = s.call(ctx)
	di, err := s.rpc.GetDescriptorInfo(c, s.cfg.Descriptor.Canonical)
	cancel()
	if err != nil {
		if down(err) {
			return unavailable(err)
		}
		return fmt.Errorf("%w: %w", ErrDescriptor, err)
	}
	if di.HasPrivateKeys || !di.IsRange || di.Descriptor == "" {
		return ErrDescriptor
	}

	// 4. the wallet's one active receive descriptor, and its next_index against the record. The allocation lock is
	// held while next_index and the recorded maximum are read, so no allocation moves either between the reads.
	select {
	case s.alloc <- struct{}{}:
		defer func() { <-s.alloc }()
	case <-ctx.Done():
		return unavailable(ctx.Err())
	}
	c, cancel = s.call(ctx)
	ds, err := s.rpc.ListDescriptors(c)
	cancel()
	if err != nil {
		if down(err) {
			return unavailable(err)
		}
		return fmt.Errorf("%w: %w", ErrDescriptor, err)
	}
	var active, found int
	var next int64
	for _, r := range ds.Descriptors {
		if r.Active && !r.Internal {
			active++
			if r.Desc == di.Descriptor {
				found++
				next = r.NextIndex
			}
		}
	}
	if active != 1 || found != 1 {
		return ErrDescriptor
	}
	max, ok, err := s.st.MaxAddressIndex(ctx)
	if err != nil {
		return err
	}
	if ok && next <= max {
		s.behind.Store(true)
		s.log.Error(logBehind)
		return ErrIndexBehind
	}
	s.mu.Lock()
	s.desc = di.Descriptor
	s.mu.Unlock()
	s.behind.Store(false)
	return nil
}
