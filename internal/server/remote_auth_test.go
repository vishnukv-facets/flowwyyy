package server

import (
	"testing"
	"time"
)

func TestHashRemoteTokenDeterministic(t *testing.T) {
	if hashRemoteToken("abc") != hashRemoteToken("abc") {
		t.Fatal("hash not deterministic")
	}
	if hashRemoteToken("abc") == hashRemoteToken("abd") {
		t.Fatal("distinct inputs collided")
	}
	if len(hashRemoteToken("abc")) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(hashRemoteToken("abc")))
	}
}

func TestMintRemoteTokenLength(t *testing.T) {
	if len(mintRemoteToken()) != 64 {
		t.Fatalf("expected 64 hex chars")
	}
}

func TestPairingStoreSingleUse(t *testing.T) {
	ps := newPairingStore()
	now := time.Unix(1_700_000_000, 0)
	code, _ := ps.createAt(now)
	if !ps.redeemAt(code, now) {
		t.Fatal("first redeem should succeed")
	}
	if ps.redeemAt(code, now) {
		t.Fatal("second redeem must fail (single-use)")
	}
}

func TestPairingStoreExpiry(t *testing.T) {
	ps := newPairingStore()
	now := time.Unix(1_700_000_000, 0)
	code, _ := ps.createAt(now)
	if ps.redeemAt(code, now.Add(pairingCodeTTL+time.Second)) {
		t.Fatal("expired code must not redeem")
	}
}

func TestPairingStoreUnknown(t *testing.T) {
	ps := newPairingStore()
	if ps.redeemAt("nope", time.Unix(1_700_000_000, 0)) {
		t.Fatal("unknown code must not redeem")
	}
}
