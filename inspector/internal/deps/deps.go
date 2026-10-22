//go:build tools

package deps

import (
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/moby/moby/api/pkg/stdcopy"
	_ "github.com/moby/moby/api/types"
	_ "github.com/moby/moby/api/types/container"
	_ "github.com/moby/moby/api/types/events"
	_ "github.com/moby/moby/api/types/network"
	_ "github.com/moby/moby/client"
	_ "github.com/rabbitmq/amqp091-go"
	_ "github.com/redis/go-redis/v9"
	_ "github.com/vmihailenco/msgpack/v5"
	_ "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	_ "go.opentelemetry.io/proto/otlp/common/v1"
	_ "go.opentelemetry.io/proto/otlp/trace/v1"
	_ "google.golang.org/protobuf/encoding/protojson"
	_ "google.golang.org/protobuf/proto"
)
