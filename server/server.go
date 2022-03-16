package server

import (
	"fmt"
	"log"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ikilobyte/netman/eventloop"

	"github.com/ikilobyte/netman/util"

	"github.com/ikilobyte/netman/iface"
)

type serverStatus = int

const (
	stopped  serverStatus = iota // 已停止（初始状态）
	started                      // 已启动
	stopping                     // 停止中
)

type Server struct {
	ip               string
	port             int
	status           serverStatus          // 状态
	options          *Options              // serve启动可选项参数
	socket           *socket               // 直接系统调用的方式监听TCP端口，不使用官方的net包
	acceptor         iface.IAcceptor       // 处理新连接
	eventloop        iface.IEventLoop      // 事件循环管理
	connectMgr       iface.IConnectManager // 所有的连接管理
	packer           iface.IPacker         // 负责封包解包
	emitCh           chan iface.IRequest   // 从这里接收epoll转发过来的消息，然后交给worker去处理
	routerMgr        *RouterMgr            // 路由统一管理
	websocketHandler iface.IWebsocketHandler
}

func (s *Server) SetWebSocketHandler(handler iface.IWebsocketHandler) {
	s.websocketHandler = handler
}

//New 创建Server
func New(protocol string, opts ...Option) *Server {

	application, ip, port := parseProtocol(protocol)
	options := parseOption(opts...)

	// 使用几个事件循环管理连接
	if options.NumEventLoop <= 0 {
		options.NumEventLoop = runtime.NumCPU()
	}

	// 封包解包的实现层，外部可以自行实现IPacker使用自己的封包解包方式
	if options.Packer == nil {
		options.Packer = util.NewDataPacker()
		options.Packer.SetMaxBodyLength(options.MaxBodyLength)
	}

	// 日志保存路径
	if options.LogOutput != nil {
		util.Logger.SetOutput(options.LogOutput)
	}

	// 应用层协议
	options.Application = application

	// 初始化
	server := &Server{
		ip:         ip,
		port:       port,
		options:    options,
		status:     stopped,
		socket:     createSocket(fmt.Sprintf("%s:%d", ip, port), options.TCPKeepAlive),
		eventloop:  eventloop.NewEventLoop(options.NumEventLoop),
		connectMgr: newConnectManager(options),
		emitCh:     make(chan iface.IRequest, 128),
		packer:     options.Packer,
		routerMgr:  NewRouterMgr(),
	}

	// 初始化epoll
	if err := server.eventloop.Init(server.connectMgr); err != nil {
		log.Panicln(err)
	}

	// 执行wait
	server.eventloop.Start(server.emitCh)
	server.acceptor = newAcceptor(
		server.packer,
		server.connectMgr,
		options,
	)

	// 接收消息的处理，
	go func() {
		for {
			select {
			case request, ok := <-server.emitCh:

				// 通道已关闭
				if !ok {
					return
				}

				// 交给路由管理中心去处理，执行业务逻辑
				if err := server.routerMgr.Do(request); err != nil {
					util.Logger.Infoln(fmt.Errorf("do handler err %s", err))
				}
			}
		}
	}()

	return server
}

//parseProtocol 解析出是否有指定的协议
func parseProtocol(protocol string) (string, string, int) {
	parts := strings.Split(protocol, ":")
	if len(parts) != 3 {
		log.Panicln("protocol fail")
	}

	kind := parts[0]
	host := strings.Trim(parts[1], "//")
	port, err := strconv.Atoi(parts[2])
	if err != nil {
		log.Panicln("port err", err)
	}

	if kind != "tcp" && kind != "websocket" {
		log.Panicln("protocol type fail")
	}

	return kind, host, port
}

//AddRouter 添加路由处理
func (s *Server) AddRouter(msgID uint32, router iface.IRouter) {
	s.routerMgr.Add(msgID, router)
}

//Start 启动
func (s *Server) Start() {
	if s.status != stopped {
		return
	}
	s.status = started

	if err := s.acceptor.Run(s.socket.fd, s.eventloop); err != nil {
		util.Logger.Errorf("server start error：%v", err)
	}
}

//Stop 停止
func (s *Server) Stop() {
	s.status = stopping
	s.connectMgr.ClearAll()
	s.eventloop.Stop()
	close(s.emitCh)
	_ = unix.Close(s.socket.fd)
	s.acceptor.Exit()
}
