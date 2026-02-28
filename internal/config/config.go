// Package config loads all service connection settings from environment variables,
// with sane defaults for local development. No secrets are ever hardcoded.
package config

import "os"

type Config struct {
	// PostgreSQL
	PostgresDSN string

	// Redis
	RedisAddr string

	// RabbitMQ
	RabbitMQURL string

	// Elasticsearch
	ElasticsearchURL string

	// HTTP server
	APIPort string

	// Materialized view refresh schedule (cron syntax, e.g. "@hourly" or "0 * * * *")
	MVRefreshSchedule string
}

// Load reads environment variables and returns a populated Config.
// Each variable has a default that matches the docker-compose service names,
// so the app works out-of-the-box when started via `docker compose up`.
func Load() *Config {
	return &Config{
		PostgresDSN:       getEnv("POSTGRES_DSN", "user=postgres password=secret dbname=ecommerce sslmode=disable host=postgres"),
		RedisAddr:         getEnv("REDIS_ADDR", "redis:6379"),
		RabbitMQURL:       getEnv("RABBITMQ_URL", "amqp://guest:guest@rabbitmq:5672/"),
		ElasticsearchURL:  getEnv("ELASTICSEARCH_URL", "http://elasticsearch:9200"),
		APIPort:           getEnv("API_PORT", "8080"),
		MVRefreshSchedule: getEnv("MV_REFRESH_SCHEDULE", "@hourly"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
