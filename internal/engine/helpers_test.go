package engine

import "time"

// timeout guards tests that could hang on a malformed graph.
func timeout() <-chan time.Time { return time.After(5 * time.Second) }
