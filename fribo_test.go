package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTokenIsReusedAndRenewedByLogin(t *testing.T) {
	f := newFakeFribo(t)
	c := f.client()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := c.Cabins(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if f.logins != 1 {
		t.Fatalf("want one login for three calls, got %d", f.logins)
	}

	// Close to expiry the client logs in again instead of using a refresh token.
	c.mu.Lock()
	c.expires = time.Now().Add(tokenMargin / 2)
	c.mu.Unlock()
	if _, err := c.Cabins(ctx); err != nil {
		t.Fatal(err)
	}
	if f.logins != 2 {
		t.Fatalf("want a new login near expiry, got %d logins", f.logins)
	}

	// A 401 leads to one new login and a retry.
	f.mu.Lock()
	f.force401 = true
	f.mu.Unlock()
	cabins, err := c.Cabins(ctx)
	if err != nil || len(cabins) != 1 || cabins[0].ID != testCabinID {
		t.Fatalf("retry after 401: %v %v", cabins, err)
	}
	if f.logins != 3 {
		t.Fatalf("want a login after 401, got %d", f.logins)
	}
}

func TestConcurrentCallsShareOneLogin(t *testing.T) {
	f := newFakeFribo(t)
	c := f.client()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Cabins(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.logins != 1 {
		t.Fatalf("want one login, got %d", f.logins)
	}
}

func TestLoginErrorExplainsAndHidesPassword(t *testing.T) {
	f := newFakeFribo(t)
	c := newClient(f.srv.URL, testEmail, "private-wrong-password")
	_, err := c.Cabins(context.Background())
	if err == nil || !strings.Contains(err.Error(), "wrong e-mail or password") {
		t.Fatalf("want a clear login error, got %v", err)
	}
	assertNoPrivate(t, "login error", []byte(err.Error()))
}

func TestCabinsOmitAddress(t *testing.T) {
	f := newFakeFribo(t)
	cabins, err := f.client().Cabins(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cabins) != 1 || cabins[0].Name != "Fjellhytta" || !cabins[0].IsDefault {
		t.Fatalf("unexpected cabins: %+v", cabins)
	}
}

func TestTokenSubject(t *testing.T) {
	if sub, err := tokenSubject(fakeJWT(testUserID, 1)); err != nil || sub != testUserID {
		t.Fatalf("subject: %q %v", sub, err)
	}
	for _, bad := range []string{"", "a.b", "a.!!!.c", fakeJWT("", 1)} {
		if _, err := tokenSubject(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
