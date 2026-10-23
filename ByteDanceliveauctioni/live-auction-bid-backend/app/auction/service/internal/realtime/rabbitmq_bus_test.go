package realtime

import "testing"

func TestNewRabbitMQBusAppliesProductionDefaults(t *testing.T) {
	bus, err := NewRabbitMQBus(RabbitMQBusConfig{
		URL:    "amqp://guest:guest@localhost:5672/",
		Origin: "node-a",
	})
	if err != nil {
		t.Fatalf("NewRabbitMQBus returned error: %v", err)
	}

	if bus.exchange != defaultRabbitMQExchange {
		t.Fatalf("exchange = %q, want %q", bus.exchange, defaultRabbitMQExchange)
	}
	if bus.routingKey != defaultRabbitMQRoutingKey {
		t.Fatalf("routing key = %q, want %q", bus.routingKey, defaultRabbitMQRoutingKey)
	}
	if bus.queue != defaultRabbitMQQueue {
		t.Fatalf("queue = %q, want %q", bus.queue, defaultRabbitMQQueue)
	}
	if bus.consumer != "node-a" {
		t.Fatalf("consumer = %q, want node-a", bus.consumer)
	}
	if bus.prefetch != defaultRabbitMQPrefetch {
		t.Fatalf("prefetch = %d, want %d", bus.prefetch, defaultRabbitMQPrefetch)
	}
	if !bus.durable {
		t.Fatal("durable = false, want true")
	}
}
