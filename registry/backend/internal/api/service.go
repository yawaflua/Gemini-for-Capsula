package api

import (
	projectv1 "github.com/yawaflua/marusya-gemini/registry/backend/gen/project/v1"
	"github.com/yawaflua/marusya-gemini/registry/backend/internal/adapters"
	"github.com/yawaflua/marusya-gemini/registry/backend/internal/auth"
)

type Service struct {
	db            adapters.DB
	authenticator *auth.Authenticator
	projectv1.UnimplementedPluginRegistryServer
}

var _ projectv1.PluginRegistryServer = (*Service)(nil)

type ServiceDependencies struct {
	DB            adapters.DB
	Authenticator *auth.Authenticator
}

func NewService(dependencies ServiceDependencies) *Service {
	return &Service{
		db:            dependencies.DB,
		authenticator: dependencies.Authenticator,
	}
}
