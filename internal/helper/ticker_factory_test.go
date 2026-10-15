package helper

import (
	"testing"
	"time"
)

func TestNullTickerFactory(t *testing.T) {
	ticker := NewNullTickerFactory().NewTicker()

	select {
	case <-ticker.C():
		t.Fatal("unexpected tick received before reset")
	default:
	}

	ticker.Reset()

	select {
	case <-ticker.C():
		t.Fatal("unexpected tick received after reset")
	default:
	}
}

func TestNewTimerTickerFactory(t *testing.T) {
	tickerFactory := NewTimerTickerFactory(time.Nanosecond)

	ticker1 := tickerFactory.NewTicker()
	ticker2 := tickerFactory.NewTicker()

	select {
	case <-ticker1.C():
		t.Fatal("unexpected tick received from ticker1 before reset")
	case <-ticker2.C():
		t.Fatal("unexpected tick received from ticker2 before reset")
	default:
	}

	ticker1.Reset()

	select {
	case <-ticker1.C():
	case <-ticker2.C():
		t.Fatal("unexpected tick received from ticker2 after resetting ticker1")
	}
}
