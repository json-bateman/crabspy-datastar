package eventbus

import (
	"testing"
)

func TestSubscibeAndNotify(t *testing.T) {
	bus := NewBus()
	ch := bus.SubscribeRoom(1)

	bus.NotifyRoom(1)

	select {
	case <-ch:
	default:
		t.Fatal("expected notification on channel")
	}
}

func TestUnsubscribe_ClosesChannel(t *testing.T) {
	bus := NewBus()
	ch := bus.SubscribeRoom(1)
	bus.UnsubscribeRoom(1, ch)

	_, open := <-ch
	if open {
		t.Fatal("channel should be closed after unsubscribe")
	}
}

func TestUnsubscribe_CleansEmptyRoom(t *testing.T) {
	bus := NewBus()
	ch := bus.SubscribeRoom(1)

	bus.UnsubscribeRoom(1, ch)

	bus.mu.Lock()
	_, exists := bus.rooms[1]
	bus.mu.Unlock()

	if exists {
		t.Fatal("Room entry should be removed when last subscriber leaves")
	}
}

func TestMultipleSubscribers(t *testing.T) {
	bus := NewBus()
	ch1 := bus.SubscribeRoom(1)
	ch2 := bus.SubscribeRoom(1)
	ch3 := bus.SubscribeRoom(1)

	bus.NotifyRoom(1)
	for _, ch := range []chan struct{}{ch1, ch2, ch3} {
		select {
		case <-ch:
		default:
			t.Fatal("One or more subscribers didn't recieve message")
		}
	}
}
