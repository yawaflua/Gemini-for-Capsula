package config

import (
	"time"

	"github.com/ilyakaznacheev/cleanenv"
	"github.com/yawaflua/marusya-gemini/registry/backend/internal/adapters/psql"
)

type C struct {
	Env string `env:"ENV" env-default:"dev"`

	Image    ImageConfig
	GRPC     GRPCConfig
	HTTP     HTTPConfig
	Auth     AuthConfig
	Postgres psql.Config
}

type AuthConfig struct {
	Secret          string        `env:"JWT_SECRET" env-required:"true"`
	Issuer          string        `env:"JWT_ISSUER" env-default:"marusya-plugins"`
	Audience        string        `env:"JWT_AUDIENCE" env-default:"marusya-plugins-api"`
	AccessTokenTTL  time.Duration `env:"JWT_ACCESS_TOKEN_TTL" env-default:"15m"`
	RefreshTokenTTL time.Duration `env:"JWT_REFRESH_TOKEN_TTL" env-default:"720h"`
	Leeway          time.Duration `env:"JWT_LEEWAY" env-default:"30s"`
}

type GRPCConfig struct {
	Host string `env:"GRPC_HOST" env-default:"localhost"`
	Port int    `env:"GRPC_PORT" env-default:"9090"`
}

type HTTPConfig struct {
	Host string `env:"HTTP_HOST" env-default:"0.0.0.0"`
	Port int    `env:"HTTP_PORT" env-default:"8080"`
	// SiteDir is the built frontend served next to the API; empty serves
	// only the API.
	SiteDir string `env:"SITE_DIR"`
}

type ImageConfig struct {
	URL string `env:"IMAGE_URL" env-default:"http://localhost"`
}

func MustLoad() *C {
	var cfg C
	err := cleanenv.ReadEnv(&cfg)

	if err != nil {
		panic(err)
	}

	return &cfg
}
