// +build darwin freebsd dragonfly

package eventloop

import (
	"fmt"
	"io"
	"time"

	"github.com/ikilobyte/netman/common"

	"github.com/ikilobyte/netman/iface"
	"github.com/ikilobyte/netman/util"
	"golang.org/x/sys/unix"
)

type Poller struct {
	Epfd       int                   // eventpoll fd
	Events     []unix.Kevent_t       //
	ConnectMgr iface.IConnectManager //
}

//NewPoller 创建kqueue
func NewPoller(connectMgr iface.IConnectManager) (*Poller, error) {

	fd, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}

	return &Poller{
		Epfd:       fd,
		Events:     make([]unix.Kevent_t, 128),
		ConnectMgr: connectMgr,
	}, nil
}

func (p *Poller) AddRead(fd int, connID int) error {
	_, err := unix.Kevent(p.Epfd, []unix.Kevent_t{
		{
			Ident:  uint64(fd),
			Filter: unix.EVFILT_READ,
			Flags:  unix.EV_ADD,
			Fflags: 0,
			Data:   int64(connID),
			Udata:  nil,
		},
	}, nil, nil)
	return err
}

func (p *Poller) AddWrite(fd, connID int) error {
	_, err := unix.Kevent(p.Epfd, []unix.Kevent_t{
		{
			Ident:  uint64(fd),
			Filter: unix.EVFILT_WRITE,
			Flags:  unix.EV_ADD,
			Fflags: 0,
			Data:   int64(connID),
			Udata:  nil,
		},
	}, nil, nil)
	return err
}

//ModWrite 将事件修改为写
func (p *Poller) ModWrite(fd, connID int) error {

	// 删除读事件
	_, err := unix.Kevent(p.Epfd, []unix.Kevent_t{
		{
			Ident:  uint64(fd),
			Filter: unix.EVFILT_READ,
			Flags:  unix.EV_DELETE,
			Fflags: 0,
			Data:   0,
			Udata:  nil,
		},
	}, nil, nil)

	if err != nil {
		return err
	}

	// 添加写事件
	return p.AddWrite(fd, connID)
}

//ModRead 将事件修改为读
func (p *Poller) ModRead(fd, connID int) error {
	// 删除写事件
	_, err := unix.Kevent(p.Epfd, []unix.Kevent_t{
		{
			Ident:  uint64(fd),
			Filter: unix.EVFILT_WRITE,
			Flags:  unix.EV_DELETE,
			Fflags: 0,
			Data:   0,
			Udata:  nil,
		},
	}, nil, nil)

	if err != nil {
		return err
	}

	// 添加读事件
	return p.AddRead(fd, connID)
}

//Wait 这里处理的是socket的读
func (p *Poller) Wait(emitCh chan<- iface.IRequest) {

	for {

		n, err := unix.Kevent(p.Epfd, nil, p.Events, nil)
		if err != nil {
			if err == unix.EINTR || err == unix.EAGAIN {
				continue
			}

			util.Logger.WithField("epfd", p.Epfd).WithField("error", err).Error("kqueue wait error")

			// 断开这个epoll管理的所有连接
			p.ConnectMgr.ClearByEpFd(p.Epfd)

			return
		}

		// 处理连接
		for i := 0; i < n; i++ {
			var (
				event  = p.Events[i]
				connFd = int(event.Ident)
				conn   iface.IConnect
			)

			// 1、通过connID获取conn实例
			if conn = p.ConnectMgr.Get(connFd); conn == nil {
				// 断开连接
				_ = unix.Close(connFd)
				_ = p.Remove(connFd)
				continue
			}

			// 判断是否为写事件
			if event.Filter == unix.EVFILT_WRITE {
				if err := p.ProceedWrite(conn); err != nil {
					_ = conn.Close()     // 断开连接
					_ = p.Remove(connFd) // 删除事件订阅
					p.ConnectMgr.Remove(conn)
					util.Logger.Errorf("kqueue do write error %v", err)
					continue
				}
				continue
			}

			// 1、判断是否开启tls，需要完成tls握手
			if conn.GetTLSEnable() && conn.GetHandshakeCompleted() == false {

				fmt.Println("开始握手！！！？？")

				time.Sleep(time.Hour)
				//tlsConn := tls.Server(conn.(net.Conn), &tls.Config{Certificates: []tls.Certificate{conn.GetCertificate()}})
				//// tls握手失败
				//if err := tlsConn.Handshake(); err != nil {
				//	p.ClearByConn(conn)
				//	util.Logger.Errorf("tls handshake error %v", err)
				//	continue
				//}

				// 1、设置状态
				conn.SetHandshakeCompleted()

				// 2、设置为非阻塞
				if err := unix.SetNonblock(connFd, true); err != nil {
					p.ClearByConn(conn)
					continue
				}

				fmt.Println("tls握手成功！")
				continue
			}

			// 2、读取一个完整的包
			message, err := conn.GetPacker().ReadFull(connFd)
			if err != nil {

				switch err {
				case io.EOF, util.HeadBytesLengthFail, util.BodyLenExceedLimit:
					// 断开连接操作
					_ = conn.Close()
					_ = p.Remove(connFd)
					p.ConnectMgr.Remove(conn)
				default:
					continue
				}
				continue
			}

			// 3、将消息传递出去，交给worker处理
			if message.Len() <= 0 {
				continue
			}

			emitCh <- util.NewRequest(conn, message)
		}
	}
}

func (p *Poller) Remove(fd int) error {
	return nil
}

func (p *Poller) Close() error {
	return unix.Close(p.Epfd)
}

//GetConnectMgr .
func (p *Poller) GetConnectMgr() iface.IConnectManager {
	return p.ConnectMgr
}

//ProceedWrite 将之前未发送完毕的数据，继续发送出去
func (p *Poller) ProceedWrite(conn iface.IConnect) error {

	// 1. 获取一个待发送的数据
	dataBuff, empty := conn.GetWriteBuff()

	// 2. 队列中没有未发送完毕的数据，将当前连接改为可读事件
	if empty {

		// 更改为可读状态
		if err := p.ModRead(conn.GetFd(), conn.GetID()); err != nil {
			return err
		}

		// 同步状态
		conn.SetState(common.EPollIN)
	}

	// 3. 发送
	n, err := unix.Write(conn.GetFd(), dataBuff)

	if err != nil {
		return err
	}

	// 设置writeBuff
	conn.SetWriteBuff(dataBuff[n:])
	return nil
}

//ClearByConn 统一入口关闭某个连接
func (p *Poller) ClearByConn(conn iface.IConnect) {
	_ = conn.Close()           // 断开连接
	_ = p.Remove(conn.GetFd()) // 删除事件订阅
	p.ConnectMgr.Remove(conn)
}
