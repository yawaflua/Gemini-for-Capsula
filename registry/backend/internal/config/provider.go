package config

import (
	"context"

	projectv1 "github.com/yawaflua/marusya-gemini/registry/backend/gen/project/v1"
	"github.com/yawaflua/marusya-gemini/registry/backend/internal/adapters"
	"github.com/yawaflua/marusya-gemini/registry/backend/internal/adapters/psql"
	"github.com/yawaflua/marusya-gemini/registry/backend/internal/api"
	"github.com/yawaflua/marusya-gemini/registry/backend/internal/auth"
)

type Provider struct {
	db *adapters.DB
	c  *C

	registryServer projectv1.PluginRegistryServer
	auth           *auth.Authenticator
}

func (a *Provider) Authenticator(ctx context.Context) *auth.Authenticator {
	if a.auth == nil {
		config := a.C().Auth
		authenticator, err := auth.New(auth.Config{
			Secret:          config.Secret,
			Issuer:          config.Issuer,
			Audience:        config.Audience,
			AccessTokenTTL:  config.AccessTokenTTL,
			RefreshTokenTTL: config.RefreshTokenTTL,
			Leeway:          config.Leeway,
		}, *a.DB(ctx))
		if err != nil {
			panic(err)
		}
		a.auth = authenticator
	}

	return a.auth
}

func (a *Provider) C() *C {
	if a.c == nil {
		a.c = MustLoad()
	}
	return a.c
}

func (a *Provider) RegistryServer(ctx context.Context) projectv1.PluginRegistryServer {
	if a.registryServer == nil {
		a.registryServer = api.NewService(api.ServiceDependencies{
			DB:            *a.DB(ctx),
			Authenticator: a.Authenticator(ctx),
		})
	}
	return a.registryServer
}

func (a *Provider) DB(ctx context.Context) *adapters.DB {
	if a.db == nil {
		db, err := psql.New(ctx, a.C().Postgres)
		if err != nil {
			panic(err)
		}
		a.db = &db
	}
	return a.db
}
