// Package lb implements backend pools and load-balancing algorithms.
//
// A Pool is a fixed list of Backends and the Balancer that picks among them.
// Pools are built once per config snapshot and never change afterwards; the
// only state that moves is atomic: each backend's in-flight counter and each
// balancer's rotation counter. Picks take no lock and allocate nothing.
package lb
