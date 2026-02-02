// SPDX-FileCopyrightText: 2025 k0s authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cplb

import (
	"context"
	"net"
	"strings"
)

const (
	bufSize = 4096
)

// UnixSocketConn is a Unix domain socket connection. Read and Write return
// the response from the corresponding operation.
type UnixSocketConn struct {
	conn net.Conn
}

// NewUnixSocketConn dials the given socket path and returns a new connection.
// readDeadline and writeDeadline are set on the connection each time it is
// established; they default to 10 seconds if zero.
func NewUnixSocketConn(ctx context.Context, socketPath string) (*UnixSocketConn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	c := &UnixSocketConn{
		conn: conn,
	}
	return c, nil
}

// Read reads from the socket and returns the response (data read).
func (c *UnixSocketConn) Read() ([]byte, error) {
	buf := make([]byte, bufSize)
	n, err := c.conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// Write writes data to the socket, then reads from the socket in case the peer
// sent a response, and returns that response.
func (c *UnixSocketConn) Write(data []byte) ([]byte, error) {
	if _, err := c.conn.Write(data); err != nil {
		return nil, err
	}
	return c.Read()
}

// Close closes the connection.
func (c *UnixSocketConn) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// birdResponseCode returns the response code from a bird CLI response (first 4 bytes of the last line).
func birdResponseCode(resp []byte) string {
	lines := strings.Split(strings.TrimSpace(string(resp)), "\n")
	if len(lines) == 0 {
		return ""
	}
	lastLine := lines[len(lines)-1]
	if len(lastLine) < 4 {
		return lastLine
	}
	return lastLine[:4]
}
