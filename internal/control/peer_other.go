//go:build !linux

package control

import (
	"errors"
	"net"
)

const peerSupported = false

func samePeer(*net.UnixConn) error {
	return errors.New("private manual control requires Linux peer credentials")
}
