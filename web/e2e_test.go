package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/gorilla/sessions"
	"golang.org/x/crypto/bcrypt"

	"crabspy/internal/eventbus"
	crabsql "crabspy/sql"
	"crabspy/sql/sqlcgen"
)

// headed runs browser tests with a visible window when CRABSPY_HEADED is set to
// a truthy value (1, true, yes). Using an env var rather than a -flag keeps
// `go test ./...` working: a custom flag would be rejected by the other
// packages' test binaries, which never register it.
var headed = isTruthy(os.Getenv("CRABSPY_HEADED"))

func isTruthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

var sharedAllocCtx context.Context

func TestMain(m *testing.M) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", !headed),
		// Suppress Chrome's password-leak and Safe Browsing interstitials so
		// they can't block form submission during tests.
		chromedp.Flag("disable-features", "PasswordLeakDetection,SafeBrowsing"),
		// Multi-browser tests keep several windows open at once. Chrome throttles
		// backgrounded windows' timers/rendering, which stalls SSE + Datastar in
		// all but the focused window (very visible when headed). Disable it.
		chromedp.Flag("disable-background-timer-throttling", true),
		chromedp.Flag("disable-backgrounding-occluded-windows", true),
		chromedp.Flag("disable-renderer-backgrounding", true),
	)
	Session = sessions.NewCookieStore([]byte("test-secret-key"))

	var cancel context.CancelFunc
	sharedAllocCtx, cancel = chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	os.Exit(m.Run())
}

func newTestServer(t *testing.T) (*httptest.Server, *sql.DB) {
	t.Helper()
	db, err := crabsql.NewDatabase(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	bus := eventbus.NewBus()
	router := setupRoutes(db, bus)

	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv, db
}

func newBrowserCtx(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := chromedp.NewContext(sharedAllocCtx)
	ctx, cancelTimeout := context.WithTimeout(ctx, timeout)
	t.Cleanup(func() {
		cancelTimeout()
		cancel()
	})
	return ctx
}

// createUser inserts a user directly into the DB, bypassing the signup UI.
func createUser(t *testing.T, db *sql.DB, username, password string) sqlcgen.User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	user, err := sqlcgen.New(db).CreateUser(context.Background(), sqlcgen.CreateUserParams{
		Username:     username,
		PasswordHash: string(hash),
		DisplayName:  username,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return user
}

// loginAs navigates to /login and authenticates as the given user.
func loginAs(t *testing.T, ctx context.Context, srv *httptest.Server, username, password string) {
	t.Helper()
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/login"),
		chromedp.WaitVisible(`#login-username`),
	); err != nil {
		screenshot(t, ctx, "loginAs-failure")
		t.Fatalf("loginAs %s: %v", username, err)
	}
	// Retry until the home splash appears — an early click before Datastar wires
	// @post('/login') is a silent no-op (see submitUntil).
	submitUntil(t, ctx, `.splash-wrapper`,
		setDatastarInput(`#login-username`, username),
		setDatastarInput(`#login-password`, password),
		chromedp.Click(`button.btn-accent`),
	)
}

// hostCreateRoom navigates to /host, creates a room named name (must be <= 10
// chars), and returns the resulting /room/<code> URL. The create button is only
// rendered after a Datastar validation round-trip, so the name fill is retried
// until it appears.
func hostCreateRoom(t *testing.T, ctx context.Context, srv *httptest.Server, name string) string {
	t.Helper()
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/host"),
		chromedp.WaitVisible(`#room-name`),
	); err != nil {
		screenshot(t, ctx, "host-room-failure")
		t.Fatalf("host room: %v", err)
	}
	submitUntil(t, ctx, `button.btn`, setDatastarInput(`#room-name`, name))
	var location string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`document.querySelector('button.btn').click()`, nil),
		chromedp.WaitVisible(`span.color-accent.font-bold`),
		chromedp.Location(&location),
	); err != nil {
		screenshot(t, ctx, "host-room-failure")
		t.Fatalf("host room: %v", err)
	}
	return location
}

// screenshot captures a PNG for debugging failing tests.
func screenshot(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	var buf []byte
	if err := chromedp.Run(ctx, chromedp.FullScreenshot(&buf, 90)); err != nil {
		t.Logf("screenshot failed: %v", err)
		return
	}
	path := filepath.Join("/tmp", name+".png")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Logf("write screenshot failed: %v", err)
		return
	}
	t.Logf("screenshot saved: %s", path)
}

// submitUntil re-runs the given fill/click actions until confirmSel becomes
// visible. Datastar loads as an async ES module (from a CDN), so its data-bind /
// data-on handlers are NOT attached the instant elements render — under load an
// interaction can land before that and be silently dropped (filled form that
// never submits, input whose signal never updates). There's no clean DOM
// readiness flag to wait on, so we retry: once Datastar is wired, one more pass
// of the actions takes effect and confirmSel appears. Re-running is safe because
// setDatastarInput sets (not appends) values and re-clicks are idempotent.
func submitUntil(t *testing.T, ctx context.Context, confirmSel string, actions ...chromedp.Action) {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		_ = chromedp.Run(ctx, actions...)
		waitCtx, cancel := context.WithTimeout(ctx, time.Second)
		lastErr = chromedp.Run(waitCtx, chromedp.WaitVisible(confirmSel))
		cancel()
		if lastErr == nil {
			return
		}
	}
	screenshot(t, ctx, "submitUntil-failure")
	t.Fatalf("submitUntil %q: %v", confirmSel, lastErr)
}

// setDatastarInput sets a Datastar data-bind input's value in one shot and
// fires the events Datastar reacts to. Prefer this over chromedp.SendKeys for
// inputs whose keydown handler triggers a debounced server re-render (e.g.
// #room-name -> /validate/host): per-key typing can race that morph and drop
// characters, especially under load. The single input event updates the bound
// signal; the keydown event kicks the debounced validation.
func setDatastarInput(sel, value string) chromedp.Action {
	js := fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		if (!el) return false;
		const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
		setter.call(el, %q);
		el.dispatchEvent(new Event('input', { bubbles: true }));
		el.dispatchEvent(new KeyboardEvent('keydown', { bubbles: true, key: 'x' }));
		return true;
	})()`, sel, value)
	return chromedp.Evaluate(js, nil)
}

func TestSignupAndLogin(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	ctx := newBrowserCtx(t, 20*time.Second)

	// Sign up — the submit button is only rendered after Datastar validation
	// passes, so retry the fill until it appears, then submit.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/signup"),
		chromedp.WaitVisible(`#signup-username`),
	); err != nil {
		screenshot(t, ctx, "signup-failure")
		t.Fatalf("signup: %v", err)
	}
	submitUntil(t, ctx, `button.btn-secondary`,
		setDatastarInput(`#signup-username`, "testuser"),
		setDatastarInput(`#signup-password`, "Cr4bSpy!x9Qz#Test"),
	)
	if err := chromedp.Run(ctx,
		chromedp.Click(`button.btn-secondary`),
		chromedp.WaitVisible(`#login-username`),
	); err != nil {
		screenshot(t, ctx, "signup-failure")
		t.Fatalf("signup: %v", err)
	}

	submitUntil(t, ctx, `.splash-wrapper`,
		setDatastarInput(`#login-username`, "testuser"),
		setDatastarInput(`#login-password`, "Cr4bSpy!x9Qz#Test"),
		chromedp.Click(`button.btn-accent`),
	)
	var location string
	if err := chromedp.Run(ctx, chromedp.Location(&location)); err != nil {
		t.Fatalf("location: %v", err)
	}
	if location != srv.URL+"/" {
		t.Errorf("expected redirect to /, got %s", location)
	}
}

func TestLoginInvalidCredentials(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	ctx := newBrowserCtx(t, 15*time.Second)

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/login"),
		chromedp.WaitVisible(`#login-username`),
	); err != nil {
		screenshot(t, ctx, "login-invalid-failure")
		t.Fatal(err)
	}
	submitUntil(t, ctx, `.color-error`,
		setDatastarInput(`#login-username`, "nobody"),
		setDatastarInput(`#login-password`, "Zx9!qWrong$Crab7"),
		chromedp.Click(`button.btn-accent`),
	)
	var errText string
	if err := chromedp.Run(ctx, chromedp.Text(`.color-error`, &errText)); err != nil {
		t.Fatal(err)
	}
	if errText == "" {
		t.Error("expected an error message to be shown")
	}
}

func TestUnauthenticatedRedirectsToLogin(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	ctx := newBrowserCtx(t, 10*time.Second)

	var location string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/"),
		chromedp.Location(&location),
	); err != nil {
		t.Fatal(err)
	}
	if location != srv.URL+"/login" {
		t.Errorf("expected redirect to /login, got %s", location)
	}
}

func TestHostRoom(t *testing.T) {
	t.Parallel()
	srv, db := newTestServer(t)
	ctx := newBrowserCtx(t, 20*time.Second)

	createUser(t, db, "host", "Cr4bSpy!x9Qz#Test")
	loginAs(t, ctx, srv, "host", "Cr4bSpy!x9Qz#Test")

	location := hostCreateRoom(t, ctx, srv, "TestRoom")
	if !strings.Contains(location, "/room/") {
		t.Errorf("expected redirect to /room/<code>, got %s", location)
	}
}

// TestRoomFull fills a room to its 8-player max, then asserts a 9th player is
// redirected to /full. (Player joining itself is covered by TestPlayThroughGame.)
func TestRoomFull(t *testing.T) {
	// Not t.Parallel: this test needs ~9 browsers at once. Running it alongside
	// the other multi-browser test spawns enough Chrome instances to starve the
	// CPU and blow the per-context timeouts. Non-parallel tests run isolated
	// (before the parallel ones resume), so it gets the machine to itself.
	const pw = "Cr4bSpy!x9Qz#Test"
	srv, db := newTestServer(t)

	// Host creates the room (occupies slot 1).
	hostCtx := newBrowserCtx(t, 20*time.Second)
	createUser(t, db, "host", pw)
	loginAs(t, hostCtx, srv, "host", pw)
	roomURL := hostCreateRoom(t, hostCtx, srv, "FullRoom")

	// Fill the remaining 7 slots (players 2-8) in parallel.
	var wg sync.WaitGroup
	for i := 2; i <= 8; i++ {
		i := i
		username := fmt.Sprintf("player%d", i)
		createUser(t, db, username, pw)
		wg.Add(1)
		go func() {
			defer wg.Done()
			pCtx := newBrowserCtx(t, 20*time.Second)
			loginAs(t, pCtx, srv, username, pw)
			if err := chromedp.Run(pCtx,
				chromedp.Navigate(roomURL),
				chromedp.WaitVisible(`span.color-accent.font-bold`),
			); err != nil {
				screenshot(t, pCtx, fmt.Sprintf("roomfull-join-%s", username))
				t.Errorf("player %s failed to join: %v", username, err)
			}
		}()
	}
	wg.Wait()

	// 9th player should be redirected to /full.
	createUser(t, db, "player9", pw)
	overCtx := newBrowserCtx(t, 20*time.Second)
	loginAs(t, overCtx, srv, "player9", pw)

	var overLocation string
	if err := chromedp.Run(overCtx,
		chromedp.Navigate(roomURL),
		chromedp.WaitVisible(`a.btn-primary[href="/"]`), // only on /full page
		chromedp.Location(&overLocation),
	); err != nil {
		screenshot(t, overCtx, "roomfull-over-max-failure")
		t.Fatal(err)
	}
	if !strings.Contains(overLocation, "/full") {
		t.Errorf("expected redirect to /full for 9th player, got %s", overLocation)
	}
}

// TestConcurrentJoinsRespectMaxPlayers fires many simultaneous join attempts at
// one room and asserts the room never exceeds its max_players. This is the race
// that happens when several people click a room link at the same instant; the
// SSE join handler relies on JoinRoomIfNotFull being atomic (count check + insert
// in a single statement) to enforce the cap. Driven at the query level so it's
// deterministic rather than a flaky fan-out of browsers.
func TestConcurrentJoinsRespectMaxPlayers(t *testing.T) {
	t.Parallel()
	const pw = "Cr4bSpy!x9Qz#Test"
	const maxPlayers = 8
	const attempts = 30 // way more joiners than slots

	db, err := crabsql.NewDatabase(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	q := sqlcgen.New(db)
	ctx := context.Background()

	host := createUser(t, db, "host", pw)
	room, err := q.CreateRoom(ctx, sqlcgen.CreateRoomParams{
		Name:          "Race",
		HostID:        host.ID,
		MaxPlayers:    maxPlayers,
		MaxLocations:  30,
		Code:          "RACE1",
		TimerDuration: 480,
	})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	userIDs := make([]int64, attempts)
	for i := range userIDs {
		userIDs[i] = createUser(t, db, fmt.Sprintf("racer%d", i), pw).ID
	}

	// Release every goroutine at once via a barrier so they contend maximally.
	var wg sync.WaitGroup
	var succeeded atomic.Int64
	start := make(chan struct{})
	for _, uid := range userIDs {
		uid := uid
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			n, err := q.JoinRoomIfNotFull(ctx, sqlcgen.JoinRoomIfNotFullParams{
				RoomID: room.ID,
				UserID: uid,
			})
			if err != nil {
				t.Errorf("JoinRoomIfNotFull: %v", err)
				return
			}
			if n == 1 {
				succeeded.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	members, err := q.GetRoomMembers(ctx, room.ID)
	if err != nil {
		t.Fatalf("GetRoomMembers: %v", err)
	}
	if len(members) != maxPlayers {
		t.Errorf("room has %d members, want %d (cap breached by concurrent joins)", len(members), maxPlayers)
	}
	if got := succeeded.Load(); got != maxPlayers {
		t.Errorf("%d joins reported success, want %d", got, maxPlayers)
	}
}

// clickButtonByText clicks the first <button> whose trimmed text equals label,
// returning true if one was found. Used to drive Datastar buttons that have no
// stable id/class of their own.
func clickButtonByText(label string) string {
	return fmt.Sprintf(`(() => {
		const btn = [...document.querySelectorAll('button')].find(b => b.textContent.trim() === %q);
		if (btn) { btn.click(); return true; }
		return false;
	})()`, label)
}

// TestPlayThroughGame logs in 8 players (host + 7 joiners), starts the game,
// has one player accuse another, and everyone votes "spy" so the round resolves
// to a finish. It doesn't matter who the spy actually is — a unanimous spy vote
// always ends the game.
func TestPlayThroughGame(t *testing.T) {
	// Not t.Parallel: spawns 8 browsers. See the note on TestRoomFull — running
	// the two multi-browser tests concurrently starves Chrome and causes
	// "context deadline exceeded" login timeouts.
	const pw = "Cr4bSpy!x9Qz#Test"
	const totalPlayers = 8 // host + 7 joiners == room MaxPlayers default

	srv, db := newTestServer(t)

	// ctxs is 1-indexed to match player numbering: ctxs[1] is the host,
	// ctxs[2] is the accuser, ctxs[3..8] are the voters.
	ctxs := make([]context.Context, totalPlayers+1)
	ctxs[1] = newBrowserCtx(t, 20*time.Second)

	// Host logs in and creates the room.
	createUser(t, db, "host", pw)
	loginAs(t, ctxs[1], srv, "host", pw)
	roomURL := hostCreateRoom(t, ctxs[1], srv, "PlayThru") // name <= 10 chars

	// The other 7 players log in and join concurrently.
	var wg sync.WaitGroup
	for i := 2; i <= totalPlayers; i++ {
		i := i
		username := fmt.Sprintf("player%d", i)
		createUser(t, db, username, pw)
		ctxs[i] = newBrowserCtx(t, 20*time.Second)
		wg.Add(1)
		go func() {
			defer wg.Done()
			loginAs(t, ctxs[i], srv, username, pw)
			if err := chromedp.Run(ctxs[i],
				chromedp.Navigate(roomURL),
				chromedp.WaitVisible(`span.color-accent.font-bold`),
			); err != nil {
				screenshot(t, ctxs[i], fmt.Sprintf("playthrough-join-%s", username))
				t.Errorf("player %s join: %v", username, err)
			}
		}()
	}
	wg.Wait()

	// Host starts the game once all 8 are in (button is enabled at >= 3 players).
	if err := chromedp.Run(ctxs[1],
		chromedp.WaitVisible(`button.btn-primary:not([disabled])`),
		chromedp.Evaluate(clickButtonByText("Start Game"), nil),
		chromedp.WaitVisible(`.location-card`), // Game view rendered
	); err != nil {
		screenshot(t, ctxs[1], "playthrough-start-failure")
		t.Fatalf("start game: %v", err)
	}

	// player2 pauses the game, which makes them the accuser.
	var accused bool
	if err := chromedp.Run(ctxs[2],
		chromedp.WaitVisible(`.border-2.rounded-2.cursor-pointer`), // pause button
		chromedp.Click(`.border-2.rounded-2.cursor-pointer`),
		chromedp.WaitVisible(`button.btn-small`), // "Accuse" buttons appear
		// Accuse the host (its card carries the .is-host class), so we know
		// exactly who is on trial and can skip that browser when voting.
		chromedp.Evaluate(`(() => {
			const btns = [...document.querySelectorAll('button')].filter(b => b.textContent.trim() === 'Accuse');
			const hostBtn = btns.find(b => b.parentElement && b.parentElement.querySelector('.player-card.is-host'));
			if (hostBtn) { hostBtn.click(); return true; }
			return false;
		})()`, &accused),
	); err != nil {
		screenshot(t, ctxs[2], "playthrough-accuse-failure")
		t.Fatalf("accuse: %v", err)
	}
	if !accused {
		screenshot(t, ctxs[2], "playthrough-accuse-notfound")
		t.Fatal("accuser could not find the host's Accuse button")
	}

	// Everyone eligible (player3..player8) votes "spy". The accuser auto-voted
	// spy on accusing, and the accused (host) doesn't vote — that's 7 votes for
	// an 8-player room, which is unanimous and ends the game.
	for i := 3; i <= totalPlayers; i++ {
		var voted bool
		if err := chromedp.Run(ctxs[i],
			chromedp.WaitVisible(`button.btn-small`), // "Spy!" / "Not!" buttons
			chromedp.Evaluate(clickButtonByText("Spy!"), &voted),
		); err != nil {
			screenshot(t, ctxs[i], fmt.Sprintf("playthrough-vote-player%d", i))
			t.Fatalf("player%d vote: %v", i, err)
		}
		if !voted {
			screenshot(t, ctxs[i], fmt.Sprintf("playthrough-vote-notfound-player%d", i))
			t.Errorf("player%d had no Spy! button to click", i)
		}
	}

	// The game should now be finished for every player. Check the host and one
	// voter both land on the finish screen.
	for _, i := range []int{1, totalPlayers} {
		if err := chromedp.Run(ctxs[i],
			chromedp.WaitVisible(`//div[contains(text(), 'Great Crabbining')]`, chromedp.BySearch),
		); err != nil {
			screenshot(t, ctxs[i], fmt.Sprintf("playthrough-finish-ctx%d", i))
			t.Fatalf("ctx %d did not reach finish screen: %v", i, err)
		}
	}
}
