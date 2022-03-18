// +build linux

package server

import (
	"log"
	"syscall"

	"github.com/ikilobyte/netman/common"

	"github.com/ikilobyte/netman/util"

	"golang.org/x/sys/unix"

	"github.com/ikilobyte/netman/eventloop"
	"github.com/ikilobyte/netman/iface"
)

//acceptor 统一处理用来处理新连接
type acceptor struct {
	packer     iface.IPacker
	connectMgr iface.IConnectManager
	poller     *eventloop.Poller
	eventfd    int
	eventbuff  []byte
	connID     int
	options    *Options
}

func newAcceptor(packer iface.IPacker, connectMgr iface.IConnectManager, options *Options) iface.IAcceptor {

	eventfd, err := unix.Eventfd(0, unix.EPOLL_CLOEXEC)
	if err != nil {
		log.Panicln(err)
	}

	poller, err := eventloop.NewPoller(connectMgr)
	if err != nil {
		log.Panicln(err)
	}

	return &acceptor{
		packer:     packer,
		connectMgr: connectMgr,
		poller:     poller,
		eventfd:    eventfd,
		eventbuff:  []byte{0, 0, 0, 0, 0, 0, 0, 1},
		connID:     -1,
		options:    options,
	}
}

//Run 启动
func (a *acceptor) Run(listenerFd int, loop iface.IEventLoop) error {

	poller, err := eventloop.NewPoller(a.connectMgr)
	if err != nil {
		return err
	}

	// 添加eventfd
	if err := poller.AddRead(a.eventfd, 0); err != nil {
		return err
	}

	// 添加listener fd
	if err := poller.AddRead(listenerFd, 1); err != nil {
		return err
	}

	for {
		n, err := unix.EpollWait(poller.Epfd, poller.Events, -1)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			return err
		}

		for i := 0; i < n; i++ {
			event := poller.Events[i]
			eventFd := int(event.Fd)

			if eventFd == a.eventfd {
				_, _ = unix.Read(eventFd, a.eventbuff)
				a.Close()
				return nil
			}

			connFd, sa, err := unix.Accept(eventFd)
			if err != nil {
				if err == syscall.Errno(9) {
					a.Close()
					return nil
				}
				util.Logger.Errorf("acceptor error: %v", err)
				continue
			}

			// 设置非阻塞，非tls状态下可以现在设置为非阻塞，如果是tls，则需要在完成tls握手后设置成非阻塞
			if !a.options.TlsEnable {
				if err := unix.SetNonblock(connFd, true); err != nil {
					_ = unix.Close(connFd)
					continue
				}
			}

			// 设置不延迟
			if err := unix.SetsockoptInt(connFd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1); err != nil {
				_ = unix.Close(connFd)
				continue
			}

			baseConnect := newBaseConnect(
				a.IncrementID(),
				connFd,
				util.SockaddrToTCPOrUnixAddr(sa),
				a.options,
			)
			var connect iface.IConnect
			if a.options.Application == common.RouterMode {
				connect = newRouterProtocol(baseConnect) // 路由模式，也可以是自定义应用层协议
			} else {
				connect = newWebsocketProtocol(baseConnect) // websocket协议
			}

			// 添加事件循环
			if err := loop.AddRead(connect); err != nil {
				_ = connect.Close()
				continue
			}

			// 添加到这里
			a.connectMgr.Add(connect)
		}
	}
}

func (a *acceptor) IncrementID() int {
	a.connID += 1
	return a.connID
}

func (a *acceptor) Close() {
	_ = a.poller.Remove(a.eventfd)
	_ = unix.Close(a.eventfd)
	_ = a.poller.Close()
}

func (a *acceptor) Exit() {
	_, _ = unix.Write(a.eventfd, a.eventbuff)
}
