package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr    string
	DatabaseUrl string
	ValkeyAddr  string

	KafkaBrokers    []string
	KafkaTopic      string
	KafkaPartitions int
	KafkaGroupID    string
	ConsumerCount   int // consumers in the group; > partitions means idle consumers

	AppPublicUrl   string // used in return_url for the shopper's browser
	AppInternalUrl string // used in notify_url for gateway -> app callbacks

	GatewayAddr      string // mock gateway listen address ("" disables it)
	GatewayPublicUrl string // shopper's browser opens payment pages here
	GatewayApiUrl    string // app calls the gateway API here
	GatewayApiKey    string
	GatewaySecret    string

	PaymentTTL time.Duration // unpaid orders expire after this and stock is released
}

func Load() Config {
	return Config{
		HTTPAddr:    env("HTTP_ADDR", ":8080"),
		DatabaseUrl: env("DATABASE_URL", "postgres://checkout:checkout@localhost:5432/checkout?sslmode=disable"),
		ValkeyAddr:  env("VALKEY_ADDR", "localhost:6379"),

		KafkaBrokers:    strings.Split(env("KAFKA_BROKERS", "localhost:29092"), ","),
		KafkaTopic:      env("KAFKA_TOPIC", "checkout.requested"),
		KafkaPartitions: envInt("KAFKA_PARTITIONS", 3),
		KafkaGroupID:    env("KAFKA_GROUP_ID", "checkout-worker"),
		ConsumerCount:   envInt("CONSUMER_COUNT", 3),

		AppPublicUrl:   env("APP_PUBLIC_URL", "http://localhost:8080"),
		AppInternalUrl: env("APP_INTERNAL_URL", "http://localhost:8080"),

		GatewayAddr:      env("GATEWAY_ADDR", ":9090"),
		GatewayPublicUrl: env("GATEWAY_PUBLIC_URL", "http://localhost:9090"),
		GatewayApiUrl:    env("GATEWAY_API_URL", "http://localhost:9090"),
		GatewayApiKey:    env("GATEWAY_API_KEY", "demo-api-key"),
		GatewaySecret:    env("GATEWAY_SECRET", "demo-webhook-secret"),

		PaymentTTL: time.Duration(envInt("PAYMENT_TTL_SECONDS", 900)) * time.Second,
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}
