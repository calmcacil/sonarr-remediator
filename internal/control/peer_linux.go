package control

import (
	"errors"
	"net"
	"os"
	"syscall"
)

const peerSupported = true

func samePeer(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var peer *syscall.Ucred
	var peerErr error
	if err = raw.Control(func(fd uintptr) {
		peer, peerErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if peerErr != nil || peer == nil || peer.Uid != uint32(os.Getuid()) {
		return errors.New("manual control peer identity rejected")
	}
	return nil
}
