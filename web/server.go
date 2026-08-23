package web

import (
	"context"
	"crabspy"
	"crabspy/internal"
	"crabspy/internal/eventbus"
	"crabspy/sql/sqlcgen"
	crand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/benbjohnson/hashfs"
	"github.com/go-chi/chi/v5"
	"github.com/gorilla/sessions"
	"github.com/starfederation/datastar-go/datastar"
)

//go:embed static/*
var StaticFS embed.FS

var (
	StaticSys = hashfs.NewFS(StaticFS)
	Session   *sessions.CookieStore
)

func StaticPath(format string, args ...any) string {
	return "/" + StaticSys.HashName(fmt.Sprintf("static/"+format, args...))
}

type contextKey string

const userIDKey contextKey = "userID"

func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, _ := Session.Get(r, "crabspy_session")
		userID, ok := session.Values["userID"].(int64)
		if !ok {
			slog.Error("requireAuth failed.", "userID", userID)
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		ctx := context.WithValue(r.Context(), userIDKey, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func setupRoutes(db *sql.DB, bus *eventbus.Bus) chi.Router {
	r := chi.NewRouter()

	// Disable buffering for reverse proxies like NGINX
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Accel-Buffering", "no")
			next.ServeHTTP(w, r)
		})
	})

	// Public web routes
	r.Get("/login", loginPage())
	r.Get("/artists", func(w http.ResponseWriter, r *http.Request) {
		Artists().Render(r.Context(), w)
	})

	r.Post("/login", login(db))
	r.Post("/logout", logout())

	r.Handle("/static/*", hashfs.FileServer(StaticSys))

	// Authenticated Routes
	r.Group(func(r chi.Router) {
		r.Use(requireAuth)
		r.Get("/", homePage(db))
		r.Get("/full", fullPage())
		r.Post("/avatar/{avatar}", changeAvatar(db))
		r.Get("/host", hostPage(db))
		r.Post("/host", host(db))
		r.Post("/validate/host", validateHost(db))
		r.Get("/room/{code}", roomPage(db))
		r.Post("/private", privateRoom(db))
		r.Post("/room/{code}/start", startGame(db, bus))
		r.Post("/room/{code}/pause", togglePause(db, bus))
		r.Post("/room/{code}/accuse/{id}", accuse(db, bus))
		r.Post("/room/{code}/vote/{id}/{vote}", vote(db, bus))
		r.Post("/room/{code}/reveal", revealLocation(db, bus))
		r.Post("/room/{code}/location/{location}", guessLocation(db, bus))
		r.Post("/room/{code}/timeup", timeup(db, bus))
		r.Post("/room/{code}/lobby", lobbyState(db, bus))
		r.Post("/room/{code}/leave", leaveRoom(db, bus))

		r.Get("/sse/room/{code}", roomSSE(db, bus))
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		NotFound().Render(r.Context(), w)
	})

	return r
}

// Tripcode derives a short stable identifier from a secret phrase:
// the same secret always maps to the same identity, so a player reclaims
// their crab by typing the same secret next visit. The prefix
// just domain-separates the hash; changing it would orphan every user.
func Tripcode(secret string) string {
	if strings.TrimSpace(secret) == "" {
		b := make([]byte, 16)
		crand.Read(b)
		return base64.RawURLEncoding.EncodeToString(b)[:10]
	}
	sum := sha256.Sum256([]byte("crabspy-trip-v1:" + secret))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:10]
}

func loginPage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		Login("").Render(r.Context(), w)
	}
}

func login(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var signals LoginSignals
		if err := json.NewDecoder(r.Body).Decode(&signals); err != nil {
			slog.Error("Error decoding signals", "Error", err)
			return
		}

		name := strings.TrimSpace(signals.Name)
		if n := len([]rune(name)); n == 0 || n > 12 {
			sse := datastar.NewSSE(w, r)
			sse.PatchElementTempl(Login("Name must be 1-12 characters."))
			return
		}

		trip := Tripcode(signals.Secret)
		q := sqlcgen.New(db)
		user, err := q.GetUserByTripcode(r.Context(), trip)
		switch {
		case err == nil:
			// Returning tripcode — refresh the name they typed this visit.
			user, err = q.UpdateUserName(r.Context(), sqlcgen.UpdateUserNameParams{
				Username:    name,
				DisplayName: name,
				ID:          user.ID,
			})
		case errors.Is(err, sql.ErrNoRows):
			user, err = q.CreateUser(r.Context(), sqlcgen.CreateUserParams{
				Username:    name,
				DisplayName: name,
				Tripcode:    trip,
			})
		}
		if err != nil {
			sse := datastar.NewSSE(w, r)
			slog.Error("Error creating/fetching user by tripcode", "err", err)
			sse.PatchElementTempl(Login("Something went wrong."))
			return
		}

		session, err := Session.Get(r, "crabspy_session")
		if err != nil {
			sse := datastar.NewSSE(w, r)
			slog.Error("Problem getting crabspy_session", "err", err)
			sse.PatchElementTempl(Login("Problem logging in"))
			return
		}
		session.Options.HttpOnly = true
		session.Options.SameSite = http.SameSiteLaxMode
		session.Values["userID"] = user.ID
		session.Save(r, w)

		slog.Debug("User logged in", "username", user.Username)
		sse := datastar.NewSSE(w, r)
		sse.Redirect("/")
	}
}

func logout() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := Session.Get(r, "crabspy_session")
		session.Options.MaxAge = -1
		session.Save(r, w)
		sse := datastar.NewSSE(w, r)
		sse.Redirect("/login")
	}
}

func homePage(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value(userIDKey).(int64)
		q := sqlcgen.New(db)
		user, _ := q.GetUserById(r.Context(), userID)
		Home(user).Render(r.Context(), w)
	}
}

func fullPage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		Full().Render(r.Context(), w)
	}
}

func changeAvatar(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value(userIDKey).(int64)
		avatar := chi.URLParam(r, "avatar")

		q := sqlcgen.New(db)

		user, err := q.UpdateUserAvatar(r.Context(), sqlcgen.UpdateUserAvatarParams{
			ID:         userID,
			CrabAvatar: avatar,
		})

		if err != nil {
			http.Error(w, "failed", 500)
			slog.Error("Error updating player avatar", "Error", err)
			return
		}

		sse := datastar.NewSSE(w, r)
		sse.PatchElementTempl(Home(user))
	}
}

func hostPage(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value(userIDKey).(int64)
		q := sqlcgen.New(db)
		user, _ := q.GetUserById(r.Context(), userID)
		Host(HostRules{NameEmpty: true}, user).Render(r.Context(), w)
	}
}

func validateHost(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var signals HostSignals
		if err := json.NewDecoder(r.Body).Decode(&signals); err != nil {
			return
		}
		userID := r.Context().Value(userIDKey).(int64)
		q := sqlcgen.New(db)
		user, _ := q.GetUserById(r.Context(), userID)
		rules := HostRules{
			NameEmpty:   len(strings.TrimSpace(signals.Name)) == 0,
			NameTooLong: len([]rune(signals.Name)) > 10,
		}
		sse := datastar.NewSSE(w, r)
		sse.PatchElementTempl(Host(rules, user))
	}
}

func host(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var signals HostSignals
		if err := json.NewDecoder(r.Body).Decode(&signals); err != nil {
			slog.Error("Error decoding signals", "Error", err)
			return
		}

		userID := r.Context().Value(userIDKey).(int64)
		sse := datastar.NewSSE(w, r)

		q := sqlcgen.New(db)
		user, err := q.GetUserById(r.Context(), userID)
		if err != nil {
			slog.Error("Error querying user from database.", "Error", err, "UserId", user.ID)
			sse.PatchElementTempl(Error("Error creating room, try logging out and back in."))
			return
		}

		maxLocations, err1 := strconv.ParseInt(signals.Locations, 10, 64)
		maxPlayers, err2 := strconv.ParseInt(signals.MaxPlayers, 10, 64)
		timerDuration, err3 := strconv.ParseInt(signals.TimerDuration, 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			slog.Error("Error converting string to int in host form.", "Error", err)
			sse.PatchElementTempl(Error("Invalid Input"))
			return
		}

		code := internal.GenerateRoomCode(5)
		room, err := q.CreateRoom(r.Context(), sqlcgen.CreateRoomParams{
			HostID:        user.ID,
			Name:          signals.Name,
			MaxPlayers:    maxPlayers,
			MaxLocations:  maxLocations,
			Code:          code,
			TimerDuration: timerDuration,
		})

		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				sse.PatchElementTempl(Error("Room name already taken."))
			} else {
				slog.Error("Database error CreateRoom()", "Error", err)
				sse.PatchElementTempl(Error("Server error on Creating Room"))
			}
			return
		}
		slog.Debug("Room created", "Room Code", room.Code)
		sse.Redirect(fmt.Sprintf("/room/%s", room.Code))
	}
}

func privateRoom(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var signals struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&signals); err != nil {
			slog.Error("Error Decoding signals", "err", err)
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}

		sse := datastar.NewSSE(w, r)
		q := sqlcgen.New(db)
		room, err := q.GetRoomByCode(r.Context(), strings.ToUpper(signals.Code))
		if err != nil {
			sse.PatchElementTempl(Error("Invalid room code."))
			return
		}

		sse.Redirect(fmt.Sprintf("/room/%s", room.Code))
	}
}

func roomPage(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		roomCode := strings.ToUpper(chi.URLParam(r, "code"))
		userID := r.Context().Value(userIDKey).(int64)
		q := sqlcgen.New(db)
		room, err := q.GetRoomByCode(r.Context(), roomCode)
		if err != nil {
			slog.Error("Error GetRoomByCode()", "err", err)
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}

		RoomPage(room, GameState{}, nil, userID).Render(r.Context(), w)
	}
}

func roomSSE(db *sql.DB, bus *eventbus.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		roomCode := chi.URLParam(r, "code")
		userID := r.Context().Value(userIDKey).(int64)
		q := sqlcgen.New(db)
		room, err := q.GetRoomByCode(r.Context(), strings.ToUpper(roomCode))
		if err != nil {
			slog.Error("Error GetRoomByCode()", "Error", err)
			return
		}
		sse := datastar.NewSSE(w, r)

		members, _ := q.GetRoomMembers(r.Context(), room.ID)
		alreadyMember := false
		for _, m := range members {
			if m.UserID == userID {
				alreadyMember = true
				break
			}
		}

		// Only new members consume a slot. The insert is atomic with the
		// max-players check, so simultaneous joins can't exceed the cap:
		// 0 rows affected means the room filled up first.
		if !alreadyMember {
			joined, err := q.JoinRoomIfNotFull(r.Context(), sqlcgen.JoinRoomIfNotFullParams{
				RoomID: room.ID,
				UserID: userID,
			})
			if err != nil {
				slog.Error("Error adding user to room", "err", err, "roomID", room.ID, "userID", userID)
			}
			if joined == 0 {
				sse.Redirect("/full")
				return
			}
		}

		ch := bus.SubscribeRoom(room.ID)
		defer bus.UnsubscribeRoom(room.ID, ch)
		bus.NotifyRoom(room.ID)
		slog.Debug("User joined room", "roomID", room.ID, "userID", userID)

		for {
			select {
			case <-ch:
				room, _ := q.GetRoomById(r.Context(), room.ID)
				members, _ := q.GetRoomMembers(r.Context(), room.ID)
				var gs GameState
				if g, err := q.GetCurrentGame(r.Context(), room.ID); err == nil {
					events, _ := q.GetGameEvents(r.Context(), g.ID)
					gs = BuildGameState(g, events)
					eventsWithUsers, _ := q.GetGameEventsWithUsers(r.Context(), g.ID)
					gs.EventsWithUsers = eventsWithUsers
				}
				sse.PatchElementTempl(RoomPage(room, gs, members, userID))
			case <-r.Context().Done():
				// Maybe add some leave logic on D/C from SSE
				slog.Debug("User disconnected from room SSE", "roomID", room.ID, "userID", userID)
				return
			}
		}
	}
}

func leaveRoom(db *sql.DB, bus *eventbus.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value(userIDKey).(int64)
		roomCode := chi.URLParam(r, "code")
		q := sqlcgen.New(db)

		room, err := q.GetRoomByCode(r.Context(), strings.ToUpper(roomCode))
		if err != nil {
			slog.Error("Error GetRoomByCode()", "Error", err)
			return
		}

		if err := q.LeaveRoom(r.Context(), sqlcgen.LeaveRoomParams{
			RoomID: room.ID,
			UserID: userID,
		}); err != nil {
			slog.Error("Error LeaveRoom()", "err", err)
			return
		}

		remaining, _ := q.GetRoomMembers(r.Context(), room.ID)
		if room.HostID == userID && len(remaining) > 0 {
			if err := q.UpdateRoomHost(r.Context(), sqlcgen.UpdateRoomHostParams{
				HostID: remaining[0].ID,
				ID:     room.ID,
			}); err != nil {
				slog.Error("Error UpdateRoomHost()", "err", err)
			}
		}

		bus.NotifyRoom(room.ID)
		sse := datastar.NewSSE(w, r)
		sse.Redirect("/")
	}
}

func startGame(db *sql.DB, bus *eventbus.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		roomCode := chi.URLParam(r, "code")
		userID := r.Context().Value(userIDKey).(int64)
		q := sqlcgen.New(db)
		room, err := q.GetRoomByCode(r.Context(), strings.ToUpper(roomCode))
		if err != nil {
			slog.Error("Error GetRoomByCode()", "Error", err)
			return
		}
		if room.HostID != userID {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		members, _ := q.GetRoomMembers(r.Context(), room.ID)
		spyID := members[rand.Intn(len(members))].ID

		// Randomize max locations
		locs := make([]crabspy.Location, len(crabspy.Locations))
		copy(locs, crabspy.Locations)
		rand.Shuffle(len(locs), func(i, j int) {
			locs[i], locs[j] = locs[j], locs[i]
		})
		locPool := locs[:room.MaxLocations]
		location := locPool[rand.Intn(int(room.MaxLocations))]

		titles := make([]string, len(locPool))
		for i, l := range locPool {
			titles[i] = l.Title
		}

		pool, _ := json.Marshal(titles)

		// Assign roles to non-spy players
		roles := make([]string, len(location.Roles))
		copy(roles, location.Roles)
		rand.Shuffle(len(roles), func(i, j int) { roles[i], roles[j] = roles[j], roles[i] })
		roleMap := make(map[string]string)
		ri := 0
		for _, m := range members {
			if m.ID == spyID {
				continue
			}
			if ri < len(roles) {
				roleMap[fmt.Sprintf("%d", m.ID)] = roles[ri]
				ri++
			}
		}
		roleJSON, _ := json.Marshal(roleMap)

		game, err := q.CreateGame(r.Context(), sqlcgen.CreateGameParams{
			RoomID:          room.ID,
			SpyID:           spyID,
			Location:        location.Title,
			LocationPool:    string(pool),
			RoleAssignments: string(roleJSON),
			TimerDuration:   room.TimerDuration,
		})
		if err != nil {
			slog.Error("Error CreateGame()", "err", err)
		}

		startedMeta, _ := json.Marshal(map[string]string{"location": location.Title})
		if err := q.InsertGameEvent(r.Context(), sqlcgen.InsertGameEventParams{
			GameID:    game.ID,
			UserID:    userID,
			EventType: "game_started",
			Metadata:  sql.NullString{String: string(startedMeta), Valid: true},
		}); err != nil {
			slog.Error("Error InsertGameEvent()", "err", err)
			return
		}

		if err := q.UpdateRoomState(r.Context(), sqlcgen.UpdateRoomStateParams{
			ID: room.ID, State: "game",
		}); err != nil {
			slog.Error("Error UpdateRoomState()", "err", err)
		}

		bus.NotifyRoom(room.ID)
	}
}
func togglePause(db *sql.DB, bus *eventbus.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value(userIDKey).(int64)
		roomCode := chi.URLParam(r, "code")
		q := sqlcgen.New(db)
		room, err := q.GetRoomByCode(r.Context(), strings.ToUpper(roomCode))
		if err != nil {
			slog.Error("Error GetRoomByCode()", "Error", err)
			return
		}

		game, state, err := getGameState(r.Context(), q, room.ID)
		if err != nil {
			slog.Error("Error getGameState()", "Error", err)
			return
		}

		eventType := "paused"
		if state.Paused {
			eventType = "unpaused"
		}

		if eventType == "paused" && state.HasPaused[userID] {
			return
		}

		if err := q.InsertGameEvent(r.Context(), sqlcgen.InsertGameEventParams{
			GameID:    game.ID,
			UserID:    userID,
			EventType: eventType,
		}); err != nil {
			slog.Error("Error InsertGameEvent()", "Error", err)
			return
		}

		bus.NotifyRoom(room.ID)
	}
}

func accuse(db *sql.DB, bus *eventbus.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value(userIDKey).(int64)
		roomCode := chi.URLParam(r, "code")
		accusedIDStr := chi.URLParam(r, "id")
		q := sqlcgen.New(db)

		room, err := q.GetRoomByCode(r.Context(), strings.ToUpper(roomCode))
		if err != nil {
			slog.Error("Error GetRoomByCode()", "Error", err)
			return
		}

		accusedID, err := strconv.ParseInt(accusedIDStr, 10, 64)
		if err != nil {
			slog.Error("Error parsing accused ID", "Error", err)
			return
		}

		game, state, err := getGameState(r.Context(), q, room.ID)
		if err != nil {
			slog.Error("Error getGameState()", "Error", err)
			return
		}

		if !state.Paused || state.PausedID != userID || state.HasAccused[userID] {
			slog.Info("Accuse not allowed", "paused", state.Paused, "pausedID", state.PausedID, "userID", userID)
			return
		}

		//Accuser automatically votes spy
		metadata, _ := json.Marshal(map[string]string{"vote": "spy"})
		if err := q.InsertGameEvent(r.Context(), sqlcgen.InsertGameEventParams{
			GameID:    game.ID,
			UserID:    userID,
			EventType: "accused",
			TargetID:  sql.NullInt64{Valid: true, Int64: accusedID},
			Metadata:  sql.NullString{Valid: true, String: string(metadata)},
		}); err != nil {
			slog.Error("Error InsertGameEvent()", "err", err)
			return
		}

		bus.NotifyRoom(room.ID)
	}
}

func vote(db *sql.DB, bus *eventbus.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value(userIDKey).(int64)
		roomCode := chi.URLParam(r, "code")
		accusedIDStr := chi.URLParam(r, "id")
		vote := chi.URLParam(r, "vote")
		q := sqlcgen.New(db)

		room, err := q.GetRoomByCode(r.Context(), strings.ToUpper(roomCode))
		if err != nil {
			slog.Error("Error GetRoomByCode()", "Error", err)
			return
		}

		game, state, err := getGameState(r.Context(), q, room.ID)
		if err != nil {
			slog.Error("Error getGameState()", "Error", err)
			return
		}
		if _, alreadyVoted := state.Votes[userID]; alreadyVoted {
			return
		}
		members, _ := q.GetRoomMembers(r.Context(), room.ID)

		accusedID, err := strconv.ParseInt(accusedIDStr, 10, 64)
		if err != nil {
			slog.Error("Error parsing accused ID", "Error", err)
			return
		}

		metadata, _ := json.Marshal(map[string]string{"vote": vote})
		if err := q.InsertGameEvent(r.Context(), sqlcgen.InsertGameEventParams{
			GameID:    game.ID,
			UserID:    userID,
			EventType: "voted",
			Metadata:  sql.NullString{Valid: true, String: string(metadata)},
			TargetID:  sql.NullInt64{Valid: true, Int64: accusedID},
		}); err != nil {
			slog.Error("Error InsertGameEvent()", "err", err)
			return
		}
		// Rebuild gameState after vote
		_, state, err = getGameState(r.Context(), q, room.ID)
		if err != nil {
			slog.Error("Error getGameState()", "Error", err)
			return
		}

		switch state.ResolveVote(len(members)) {
		case VoteSpyCaught:
			q.AddPointsToAllExcept(r.Context(), sqlcgen.AddPointsToAllExceptParams{
				Points: 1,
				UserID: game.SpyID,
				RoomID: room.ID,
			})
			if state.PausedID != 0 {
				q.AddPointsToMember(r.Context(), sqlcgen.AddPointsToMemberParams{
					Points: 1,
					UserID: state.PausedID,
					RoomID: room.ID,
				})
			}
			q.UpdateRoomState(r.Context(), sqlcgen.UpdateRoomStateParams{
				ID: room.ID, State: "finish",
			})
		case VoteWrongPlayer:
			q.AddPointsToMember(r.Context(), sqlcgen.AddPointsToMemberParams{
				Points: 4,
				UserID: state.SpyID,
				RoomID: room.ID,
			})
			q.UpdateRoomState(r.Context(), sqlcgen.UpdateRoomStateParams{
				ID: room.ID, State: "finish",
			})
		case VoteNoConsensus:
			q.InsertGameEvent(r.Context(), sqlcgen.InsertGameEventParams{
				GameID:    game.ID,
				UserID:    0,
				EventType: "unpaused",
			})
		case VoteIncomplete:
			// Fall through
		}

		bus.NotifyRoom(room.ID)
	}
}

func revealLocation(db *sql.DB, bus *eventbus.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value(userIDKey).(int64)
		roomCode := chi.URLParam(r, "code")
		q := sqlcgen.New(db)

		room, err := q.GetRoomByCode(r.Context(), strings.ToUpper(roomCode))
		if err != nil {
			slog.Error("Error GetRoomByCode()", "Error", err)
			return
		}

		game, _, err := getGameState(r.Context(), q, room.ID)
		if err != nil {
			slog.Error("Error getGameState()", "Error", err)
			return
		}
		if err := q.InsertGameEvent(r.Context(), sqlcgen.InsertGameEventParams{
			GameID:    game.ID,
			UserID:    userID,
			EventType: "location_revealed",
		}); err != nil {
			slog.Error("Error InsertGameEvent()", "err", err)
			return
		}

		if err := q.UpdateRoomState(r.Context(), sqlcgen.UpdateRoomStateParams{
			ID: room.ID, State: "reveal",
		}); err != nil {
			slog.Error("Error UpdateRoomState()", "err", err)
		}
		bus.NotifyRoom(room.ID)
	}
}

func guessLocation(db *sql.DB, bus *eventbus.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value(userIDKey).(int64)
		roomCode := chi.URLParam(r, "code")
		location := chi.URLParam(r, "location")
		q := sqlcgen.New(db)

		room, err := q.GetRoomByCode(r.Context(), strings.ToUpper(roomCode))
		if err != nil {
			slog.Error("Error GetRoomByCode()", "Error", err)
			return
		}

		game, _, err := getGameState(r.Context(), q, room.ID)
		if err != nil {
			slog.Error("Error getGameState()", "Error", err)
			return
		}

		//Spy wins! +4 points to the spy
		if location == game.Location {
			q.AddPointsToMember(r.Context(), sqlcgen.AddPointsToMemberParams{
				Points: 4,
				UserID: userID,
				RoomID: room.ID,
			})
		} else {
			// Everyone else wins
			q.AddPointsToAllExcept(r.Context(), sqlcgen.AddPointsToAllExceptParams{
				Points: 1,
				UserID: userID,
				RoomID: room.ID,
			})
		}

		metadata, _ := json.Marshal(map[string]string{"location": location})
		if err := q.InsertGameEvent(r.Context(), sqlcgen.InsertGameEventParams{
			GameID:    game.ID,
			UserID:    userID,
			EventType: "location_guessed",
			Metadata:  sql.NullString{Valid: true, String: string(metadata)},
		}); err != nil {
			slog.Error("Error InsertGameEvent()", "err", err)
			return
		}
		if err := q.UpdateRoomState(r.Context(), sqlcgen.UpdateRoomStateParams{
			ID: room.ID, State: "finish",
		}); err != nil {
			slog.Error("Error UpdateRoomState()", "err", err)
		}
		bus.NotifyRoom(room.ID)
	}
}

func timeup(db *sql.DB, bus *eventbus.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		roomCode := chi.URLParam(r, "code")
		q := sqlcgen.New(db)

		room, err := q.GetRoomByCode(r.Context(), strings.ToUpper(roomCode))
		if err != nil {
			slog.Error("Error GetRoomByCode()", "Error", err)
			return
		}

		_, state, err := getGameState(r.Context(), q, room.ID)
		if err != nil {
			slog.Error("Error getGameState()", "Error", err)
			return
		}
		if room.State != "game" {
			return
		}
		if state.TimerRemaining() > 0 {
			return
		}

		// Atomic: only one request transitions from game -> finish
		res, err := q.UpdateRoomStateIf(r.Context(), sqlcgen.UpdateRoomStateIfParams{
			State:   "finish",
			ID:      room.ID,
			State_2: "game",
		})
		if err != nil {
			slog.Error("Error UpdateRoomStateIf()", "err", err)
			return
		}
		rows, _ := res.RowsAffected()
		if rows == 0 {
			return // another request already transitioned
		}

		q.AddPointsToMember(r.Context(), sqlcgen.AddPointsToMemberParams{
			Points: 4,
			UserID: state.SpyID,
			RoomID: room.ID,
		})
		bus.NotifyRoom(room.ID)

	}
}

func lobbyState(db *sql.DB, bus *eventbus.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		roomCode := chi.URLParam(r, "code")
		q := sqlcgen.New(db)

		room, err := q.GetRoomByCode(r.Context(), strings.ToUpper(roomCode))
		if err != nil {
			slog.Error("Error GetRoomByCode()", "Error", err)
			return
		}

		if err := q.UpdateRoomState(r.Context(), sqlcgen.UpdateRoomStateParams{
			ID: room.ID, State: "lobby",
		}); err != nil {
			slog.Error("Error UpdateRoomState()", "err", err)
		}
		bus.NotifyRoom(room.ID)
	}
}

// RunBlocking sets up routes and deps. Starts the server. Handles cleanup
func RunBlocking(setupCtx context.Context, db *sql.DB) error {
	Session = sessions.NewCookieStore([]byte(crabspy.Env.CookieStoreSecret))
	bus := eventbus.NewBus()
	router := setupRoutes(db, bus)

	go func() {
		q := sqlcgen.New(db)
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := q.DeleteOldRooms(context.Background()); err != nil {
					slog.Error("Error cleaning up old rooms", "err", err)
				} else {
					slog.Debug("Old rooms cleaned up")
				}
			case <-setupCtx.Done():
				return
			}
		}
	}()

	addr := fmt.Sprintf(":%d", crabspy.Env.Port)
	srv := http.Server{
		Addr:    addr,
		Handler: router,
	}

	go func() {
		<-setupCtx.Done()
		log.Printf("🦀🔍 shutdown")
		if err := srv.Shutdown(context.Background()); err != nil {
			log.Printf("Error shutting down server: %v", err)
		}
	}()

	log.Printf("Starting server on http://localhost%s", addr)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("Error starting server: %v", err)
	}
	return nil
}
