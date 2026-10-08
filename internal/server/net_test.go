package server_test

import "net"

type net0 struct{}

func (net0) listen() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
