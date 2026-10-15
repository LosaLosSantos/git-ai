package helper

import "time"

// TickerFactory constructs a new ticker.
type TickerFactory interface {
	// NewTicker returns a new Ticker.
	NewTicker() Ticker
}

// TickerFactoryFunc is a function that implements TickerFactory
type TickerFactoryFunc func() Ticker

// NewTicker returns a new ticker.
func (fn TickerFactoryFunc) NewTicker() Ticker { return fn() }

// NewNullTickerFactory returns new tickers that don't tick.
func NewNullTickerFactory() TickerFactory {
	return TickerFactoryFunc(func() Ticker {
		return NewManualTicker()
	})
}

// NewTimerTickerFactory returns new tickers that tick after the given interval.
func NewTimerTickerFactory(interval time.Duration) TickerFactory {
	return TickerFactoryFunc(func() Ticker {
		return NewTimerTicker(interval)
	})
}
