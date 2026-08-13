package engine

import "testing"

// A browser origin is only meaningful for something that speaks HTTP.
func TestDatastorePortsAreNotGivenBrowserOrigins(t *testing.T) {
	for _, p := range []int{5432, 3306, 6379, 27017, 5672, 9092, 11211, 1433, 2181} {
		if httpRoutable(p) {
			t.Errorf("port %d was routed; a browser origin for it can only fail", p)
		}
	}
	for _, p := range []int{80, 443, 3000, 8080, 4100, 5173, 9000} {
		if !httpRoutable(p) {
			t.Errorf("port %d was not routed; unrecognized ports must keep their hostname", p)
		}
	}
}
