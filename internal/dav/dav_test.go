package dav

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/CaffeinatedTech/jmap-bridge/test/fixturecarddav"
)

func vcard(uid, fn string) string {
	return fmt.Sprintf("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:%s\r\nFN:%s\r\nEND:VCARD\r\n", uid, fn)
}

// openFixture starts a tier and opens a session against it.
func openFixture(t *testing.T, tier fixturecarddav.Tier) (*fixturecarddav.Server, *Session) {
	t.Helper()
	fx := fixturecarddav.New(tier, "u", "p")
	t.Cleanup(fx.Close)
	sess, err := Open(context.Background(), Config{URL: fx.URL(), Username: "u", Password: "p"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return fx, sess
}

func TestOpenDiscoveryAllTiers(t *testing.T) {
	for _, tier := range []fixturecarddav.Tier{
		fixturecarddav.TierSync, fixturecarddav.TierGetctag, fixturecarddav.TierBare,
	} {
		name := map[fixturecarddav.Tier]string{
			fixturecarddav.TierSync: "sync", fixturecarddav.TierGetctag: "getctag",
			fixturecarddav.TierBare: "bare",
		}[tier]
		t.Run(name, func(t *testing.T) {
			fx, sess := openFixture(t, tier)
			fx.AddBook("/test/work.vcf/")
			books, err := sess.Books(context.Background())
			if err != nil {
				t.Fatalf("books: %v", err)
			}
			if len(books) != 2 {
				t.Fatalf("books = %d, want 2: %+v", len(books), books)
			}
			for _, b := range books {
				if !strings.HasSuffix(b.Href, ".vcf/") {
					t.Errorf("book href = %q", b.Href)
				}
				if !b.MayWrite {
					t.Errorf("book %q not writable from privileges", b.Href)
				}
			}
			switch tier {
			case fixturecarddav.TierSync:
				if books[0].CTag == "" || books[0].SyncToken == "" {
					t.Errorf("sync tier must report ctag+token: %+v", books[0])
				}
			case fixturecarddav.TierGetctag:
				if books[0].CTag == "" {
					t.Errorf("getctag tier must report ctag: %+v", books[0])
				}
			}
		})
	}
}

func TestSyncBookIncremental(t *testing.T) {
	fx, sess := openFixture(t, fixturecarddav.TierSync)
	ctx := context.Background()
	home := "/test/addresses.vcf/"
	fx.PutCard(home, "a.vcf", vcard("uid-a", "A"))

	out, err := sess.SyncBook(ctx, home, "")
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if !out.Supported {
		t.Fatal("sync tier must report supported")
	}
	if len(out.Changed) != 1 || out.Changed[0].Href != home+"a.vcf" {
		t.Fatalf("changed = %+v", out.Changed)
	}
	if out.SyncToken == "" {
		t.Fatal("no sync token returned")
	}

	// A second pass with the new token reports nothing.
	out2, err := sess.SyncBook(ctx, home, out.SyncToken)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(out2.Changed) != 0 || len(out2.Deleted) != 0 {
		t.Fatalf("idle pass moved: %+v", out2)
	}

	// Change + delete surface on the next replay.
	fx.PutCard(home, "b.vcf", vcard("uid-b", "B"))
	fx.DeleteCard(home, "a.vcf")
	out3, err := sess.SyncBook(ctx, home, out.SyncToken)
	if err != nil {
		t.Fatalf("third sync: %v", err)
	}
	var changed, deleted []string
	for _, c := range out3.Changed {
		changed = append(changed, c.Href)
	}
	deleted = out3.Deleted
	if len(changed) != 1 || changed[0] != home+"b.vcf" {
		t.Errorf("changed = %v", changed)
	}
	if len(deleted) != 1 || deleted[0] != home+"a.vcf" {
		t.Errorf("deleted = %v", deleted)
	}
}

func TestSyncUnsupportedFallsBackToCTag(t *testing.T) {
	for _, tier := range []fixturecarddav.Tier{fixturecarddav.TierGetctag, fixturecarddav.TierBare} {
		t.Run(fixtureTier(tier), func(t *testing.T) {
			fx, sess := openFixture(t, tier)
			ctx := context.Background()
			home := "/test/addresses.vcf/"
			fx.PutCard(home, "a.vcf", vcard("uid-a", "A"))
			out, err := sess.SyncBook(ctx, home, "http://example/whatever")
			if err != nil {
				t.Fatalf("syncbook: %v", err)
			}
			if out.Supported {
				t.Fatal("fallback tier must report unsupported")
			}
			switch tier {
			case fixturecarddav.TierGetctag:
				if out.CTag == "" {
					t.Error("getctag tier must yield a ctag")
				}
			case fixturecarddav.TierBare:
				if out.CTag != "" {
					t.Errorf("bare tier must yield no ctag, got %q", out.CTag)
				}
			}
		})
	}
}

func fixtureTier(tier fixturecarddav.Tier) string {
	switch tier {
	case fixturecarddav.TierSync:
		return "sync"
	case fixturecarddav.TierGetctag:
		return "getctag"
	default:
		return "bare"
	}
}

func TestListAllEtags(t *testing.T) {
	fx, sess := openFixture(t, fixturecarddav.TierGetctag)
	ctx := context.Background()
	home := "/test/addresses.vcf/"
	fx.PutCard(home, "a.vcf", vcard("uid-a", "A"))
	fx.PutCard(home, "b.vcf", vcard("uid-b", "B"))
	metas, ctag, err := sess.ListAll(ctx, home)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("metas = %+v", metas)
	}
	if ctag == "" {
		t.Error("getctag tier must answer a ctag for a listing")
	}
}

func TestMultiGetAndGet(t *testing.T) {
	fx, sess := openFixture(t, fixturecarddav.TierSync)
	ctx := context.Background()
	home := "/test/addresses.vcf/"
	e1 := fx.PutCard(home, "a.vcf", vcard("uid-a", "A"))
	cards, err := sess.MultiGet(ctx, home, []string{home + "a.vcf", home + "missing.vcf"})
	if err != nil {
		t.Fatalf("multiget: %v", err)
	}
	if len(cards) != 1 || cards[0].ETag != e1 || !strings.Contains(string(cards[0].Raw), "UID:uid-a") {
		t.Fatalf("cards = %+v", cards)
	}
	c, err := sess.Get(ctx, home+"a.vcf")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if c.ETag != e1 {
		t.Errorf("get etag = %q, want %q", c.ETag, e1)
	}

	// Bare tier: no multiget — ErrUnsupported so callers GET one by one.
	fxb, sessb := openFixture(t, fixturecarddav.TierBare)
	fxb.PutCard(home, "a.vcf", vcard("uid-a", "A"))
	if _, err := sessb.MultiGet(ctx, home, []string{home + "a.vcf"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("bare multiget err = %v, want ErrUnsupported", err)
	}
	if _, err := sessb.Get(ctx, home+"a.vcf"); err != nil {
		t.Errorf("bare get: %v", err)
	}
}

func TestConditionalPutDelete(t *testing.T) {
	_, sess := openFixture(t, fixturecarddav.TierSync)
	ctx := context.Background()
	home := "/test/addresses.vcf/"

	etag, err := sess.Put(ctx, home+"c.vcf", []byte(vcard("uid-c", "C")), "*", "")
	if err != nil {
		t.Fatalf("create put: %v", err)
	}
	// Collision → ErrPrecondition (FR-P.8 "exists").
	if _, err := sess.Put(ctx, home+"c.vcf", []byte("x"), "*", ""); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("collision err = %v, want ErrPrecondition", err)
	}
	// Stale If-Match → ErrPrecondition (FR-P.9's overwrite test).
	if _, err := sess.Put(ctx, home+"c.vcf", []byte("x"), "", "\"stale-etag\""); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale put err = %v, want ErrPrecondition", err)
	}
	// Good If-Match → fresh etag.
	etag2, err := sess.Put(ctx, home+"c.vcf", []byte(vcard("uid-c", "C2")), "", etag)
	if err != nil {
		t.Fatalf("update put: %v", err)
	}
	if etag2 == etag {
		t.Error("etag did not change on update")
	}
	// Delete with the right etag; second delete → ErrGone.
	if err := sess.Delete(ctx, home+"c.vcf", etag2); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := sess.Delete(ctx, home+"c.vcf", etag2); !errors.Is(err, ErrGone) {
		t.Fatalf("double delete err = %v, want ErrGone", err)
	}
}

func TestDeniedWithoutCreds(t *testing.T) {
	fx := fixturecarddav.New(fixturecarddav.TierSync, "u", "right")
	defer fx.Close()
	_, err := Open(context.Background(), Config{URL: fx.URL(), Username: "u", Password: "wrong"})
	if err == nil {
		t.Fatal("open with wrong creds must fail")
	}
	if !strings.Contains(err.Error(), "401") && !errors.Is(err, ErrDenied) {
		t.Fatalf("err = %v, want 401-shaped", err)
	}
}

func TestWellKnownRedirect(t *testing.T) {
	fx := fixturecarddav.New(fixturecarddav.TierSync, "", "")
	defer fx.Close()
	sess, err := Open(context.Background(), Config{URL: fx.URL() + "/.well-known/carddav"})
	if err != nil {
		t.Fatalf("open via well-known: %v", err)
	}
	if _, err := sess.Books(context.Background()); err != nil {
		t.Fatalf("books after redirect: %v", err)
	}
}
