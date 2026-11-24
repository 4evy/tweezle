package storage

import "golang.org/x/sys/unix"

// OFD locks conflict with Chromium's fcntl locks but survive other Go
// descriptors for LOCK being closed, including goleveldb's flock handle
const levelDBRecordLockCommand = unix.F_OFD_SETLK
