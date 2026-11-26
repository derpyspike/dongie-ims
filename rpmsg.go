// SPDX-License-Identifier: GPL-3.0-only

package main

// The MSM8916 modem has a second QMI control channel, DATA6_CNTL, whose data
// goes to bam-dmux channel 1 (wwan1). VoLTE runs its IMS PDN there, separate
// from ModemManager's internet connection on DATA5 (wwan0qmi0 / wwan0).
// The mainline kernel has no QMI device for it, so we open one through
// rpmsg_char: RPMSG_CREATE_EPT_IOCTL on the modem edge's /dev/rpmsg_ctrlN gives
// /dev/rpmsgN. Idea from damonto/sigmo's "Qualcomm 410" layout (MIT), which
// uses downstream kernels' /dev/qmi_rmnet1.
//
// The endpoint resets the channel (clients, data format, calls) when the last
// file descriptor closes, so one session keeps it open the whole time.
// ModemManager must ignore /dev/rpmsgN (deploy/78-mm-ignore-rpmsg-ept.rules),
// otherwise it makes DATA6 its primary port and LTE data stops.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/damonto/wwan-go/cdcwdm"
	"golang.org/x/sys/unix"
)

const data6Channel = "DATA6_CNTL"

// _IOW(0xb5, 0x1, struct rpmsg_endpoint_info{char name[32]; u32 src; u32 dst;})
const rpmsgCreateEptIoctl = 1<<30 | 40<<16 | 0xb5<<8 | 1

type rpmsgEndpointInfo struct {
	Name [32]byte
	Src  uint32
	Dst  uint32
}

func readTrim(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
}

// existingEndpoint returns /dev/rpmsgN if an endpoint for channel exists.
func existingEndpoint(channel string) string {
	paths, _ := filepath.Glob("/sys/class/rpmsg/rpmsg[0-9]*")
	for _, p := range paths {
		if readTrim(filepath.Join(p, "name")) == channel {
			return "/dev/" + filepath.Base(p)
		}
	}
	return ""
}

// modemCtrl finds the rpmsg_ctrl device of the SMD edge that carries channel
// (e.g. remoteproc0:smd-edge.DATA6_CNTL.-1.-1 -> the ctrl of remoteproc0:smd-edge).
func modemCtrl(channel string) (string, error) {
	devs, _ := filepath.Glob("/sys/bus/rpmsg/devices/*." + channel + ".*")
	if len(devs) == 0 {
		return "", fmt.Errorf("no %s channel on the modem", channel)
	}
	edge := strings.SplitN(filepath.Base(devs[0]), "."+channel+".", 2)[0]
	ctrls, _ := filepath.Glob("/sys/class/rpmsg/rpmsg_ctrl*")
	for _, c := range ctrls {
		target, err := filepath.EvalSymlinks(filepath.Join(c, "device"))
		if err == nil && strings.HasPrefix(filepath.Base(target), edge+".rpmsg_ctrl") {
			return "/dev/" + filepath.Base(c), nil
		}
	}
	return "", fmt.Errorf("no rpmsg_ctrl for edge %s", edge)
}

// data6Endpoint returns the /dev/rpmsgN for DATA6_CNTL, creating it if needed.
func data6Endpoint() (string, error) {
	if dev := existingEndpoint(data6Channel); dev != "" {
		return dev, nil
	}
	ctrl, err := modemCtrl(data6Channel)
	if err != nil {
		return "", err
	}
	fd, err := unix.Open(ctrl, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", ctrl, err)
	}
	defer unix.Close(fd)
	info := rpmsgEndpointInfo{Src: 0xffffffff, Dst: 0xffffffff}
	copy(info.Name[:], data6Channel)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), rpmsgCreateEptIoctl, uintptr(unsafe.Pointer(&info))); errno != 0 {
		return "", fmt.Errorf("creating %s endpoint on %s: %w", data6Channel, ctrl, errno)
	}
	for i := 0; i < 50; i++ {
		if dev := existingEndpoint(data6Channel); dev != "" {
			if _, err := os.Stat(dev); err == nil {
				return dev, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", errors.New("endpoint created but its device node never appeared")
}

// rpmsgConn adapts an rpmsg endpoint to wwan-go's QMI framing: rpmsg returns
// one whole message per read() and drops what doesn't fit, while wwan-go
// reads a 3-byte header first and then the rest.
type rpmsgConn struct {
	*cdcwdm.Conn
	mu  sync.Mutex
	buf []byte
}

func (c *rpmsgConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) == 0 {
		b := make([]byte, 65536)
		n, err := c.Conn.Read(b)
		if err != nil {
			return 0, err
		}
		c.buf = b[:n]
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}
