package httpmw_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/cryptorand"
	"github.com/coder/coder/database"
	"github.com/coder/coder/database/databasefake"
	"github.com/coder/coder/httpmw"
)

func TestWorkspaceHistoryParam(t *testing.T) {
	t.Parallel()

	setupAuthentication := func(db database.Store) (*http.Request, database.Workspace) {
		var (
			id, secret = randomAPIKeyParts()
			hashed     = sha256.Sum256([]byte(secret))
		)
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(&http.Cookie{
			Name:  httpmw.AuthCookie,
			Value: fmt.Sprintf("%s-%s", id, secret),
		})
		userID, err := cryptorand.String(16)
		require.NoError(t, err)
		username, err := cryptorand.String(8)
		require.NoError(t, err)
		user, err := db.InsertUser(r.Context(), database.InsertUserParams{
			ID:             userID,
			Email:          "testaccount@coder.com",
			Name:           "example",
			LoginType:      database.LoginTypeBuiltIn,
			HashedPassword: hashed[:],
			Username:       username,
			CreatedAt:      database.Now(),
			UpdatedAt:      database.Now(),
		})
		require.NoError(t, err)
		_, err = db.InsertAPIKey(r.Context(), database.InsertAPIKeyParams{
			ID:           id,
			UserID:       user.ID,
			HashedSecret: hashed[:],
			LastUsed:     database.Now(),
			ExpiresAt:    database.Now().Add(time.Minute),
		})
		require.NoError(t, err)
		workspace, err := db.InsertWorkspace(context.Background(), database.InsertWorkspaceParams{
			ID:        uuid.New(),
			ProjectID: uuid.New(),
			OwnerID:   user.ID,
			Name:      "potato",
		})
		require.NoError(t, err)

		ctx := chi.NewRouteContext()
		ctx.URLParams.Add("user", userID)
		ctx.URLParams.Add("workspace", workspace.Name)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, ctx))
		return r, workspace
	}

	t.Run("None", func(t *testing.T) {
		t.Parallel()
		db := databasefake.New()
		rtr := chi.NewRouter()
		rtr.Use(
			httpmw.ExtractAPIKey(db, nil),
			httpmw.ExtractUserParam(db),
			httpmw.ExtractWorkspaceParam(db),
			httpmw.ExtractWorkspaceHistoryParam(db),
		)
		rtr.Get("/", nil)
		r, _ := setupAuthentication(db)
		rw := httptest.NewRecorder()
		rtr.ServeHTTP(rw, r)

		res := rw.Result()
		defer res.Body.Close()
		require.Equal(t, http.StatusBadRequest, res.StatusCode)
	})

	t.Run("NotFound", func(t *testing.T) {
		t.Parallel()
		db := databasefake.New()
		rtr := chi.NewRouter()
		rtr.Use(
			httpmw.ExtractAPIKey(db, nil),
			httpmw.ExtractUserParam(db),
			httpmw.ExtractWorkspaceParam(db),
			httpmw.ExtractWorkspaceHistoryParam(db),
		)
		rtr.Get("/", nil)

		r, _ := setupAuthentication(db)
		chi.RouteContext(r.Context()).URLParams.Add("workspacehistory", "nothin")
		rw := httptest.NewRecorder()
		rtr.ServeHTTP(rw, r)

		res := rw.Result()
		defer res.Body.Close()
		require.Equal(t, http.StatusNotFound, res.StatusCode)
	})

	t.Run("WorkspaceHistory", func(t *testing.T) {
		t.Parallel()
		db := databasefake.New()
		rtr := chi.NewRouter()
		rtr.Use(
			httpmw.ExtractAPIKey(db, nil),
			httpmw.ExtractUserParam(db),
			httpmw.ExtractWorkspaceParam(db),
			httpmw.ExtractWorkspaceHistoryParam(db),
		)
		rtr.Get("/", func(rw http.ResponseWriter, r *http.Request) {
			_ = httpmw.WorkspaceHistoryParam(r)
			rw.WriteHeader(http.StatusOK)
		})

		r, workspace := setupAuthentication(db)
		workspaceHistory, err := db.InsertWorkspaceHistory(context.Background(), database.InsertWorkspaceHistoryParams{
			ID:          uuid.New(),
			WorkspaceID: workspace.ID,
			Name:        "moo",
		})
		require.NoError(t, err)
		chi.RouteContext(r.Context()).URLParams.Add("workspacehistory", workspaceHistory.Name)
		rw := httptest.NewRecorder()
		rtr.ServeHTTP(rw, r)

		res := rw.Result()
		defer res.Body.Close()
		require.Equal(t, http.StatusOK, res.StatusCode)
	})

	t.Run("WorkspaceHistoryLatest", func(t *testing.T) {
		t.Parallel()
		db := databasefake.New()
		rtr := chi.NewRouter()
		rtr.Use(
			httpmw.ExtractAPIKey(db, nil),
			httpmw.ExtractUserParam(db),
			httpmw.ExtractWorkspaceParam(db),
			httpmw.ExtractWorkspaceHistoryParam(db),
		)
		rtr.Get("/", func(rw http.ResponseWriter, r *http.Request) {
			_ = httpmw.WorkspaceHistoryParam(r)
			rw.WriteHeader(http.StatusOK)
		})

		r, workspace := setupAuthentication(db)
		_, err := db.InsertWorkspaceHistory(context.Background(), database.InsertWorkspaceHistoryParams{
			ID:          uuid.New(),
			WorkspaceID: workspace.ID,
			Name:        "moo",
		})
		require.NoError(t, err)
		chi.RouteContext(r.Context()).URLParams.Add("workspacehistory", "latest")
		rw := httptest.NewRecorder()
		rtr.ServeHTTP(rw, r)

		res := rw.Result()
		defer res.Body.Close()
		require.Equal(t, http.StatusOK, res.StatusCode)
	})
}
