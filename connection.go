package tao

import (
  "bytes"
  "log"
  "net"
  "encoding/binary"
  "errors"
  "sync"
)

const (
  NTYPE = 4
  NLEN = 4
  MAXLEN = 1 << 23  // 8M
)

var ErrorWouldBlock error = errors.New("Would block")

type TcpConnection struct {
  conn *net.TCPConn
  name string
  closeOnce sync.Once
  wg *sync.WaitGroup
  messageSendChan chan Message
  handlerRecvChan chan ProtocolHandler
  closeConnChan chan struct{}
  onConnect onConnectCallbackType
  onMessage onMessageCallbackType
  onClose onCloseCallbackType
  onError onErrorCallbackType
}

func NewTcpConnection(s *TcpServer, c *net.TCPConn) *TcpConnection {
  tcpConn := &TcpConnection {
    conn: c,
    wg: &sync.WaitGroup{},
    messageSendChan: make(chan Message, 1024), // todo: make it configurable
    handlerRecvChan: make(chan ProtocolHandler, 1024), // todo: make it configurable
    closeConnChan: make(chan struct{}),
  }
  if s != nil {
    tcpConn.SetOnConnectCallback(s.onConnect)
    tcpConn.SetOnMessageCallback(s.onMessage)
    tcpConn.SetOnErrorCallback(s.onError)
    tcpConn.SetOnCloseCallback(s.onClose)
  }
  return tcpConn
}

func (client *TcpConnection) SetOnConnectCallback(cb func() bool) {
  if cb != nil {
    client.onConnect = onConnectCallbackType(cb)
  }
}

func (client *TcpConnection) SetOnMessageCallback(cb func(Message, *TcpConnection)) {
  if cb != nil {
    client.onMessage = onMessageCallbackType(cb)
  }
}

func (client *TcpConnection) SetOnErrorCallback(cb func()) {
  if cb != nil {
    client.onError = onErrorCallbackType(cb)
  }
}

func (client *TcpConnection) SetOnCloseCallback(cb func(*TcpConnection)) {
  if cb != nil {
    client.onClose = onCloseCallbackType(cb)
  }
}

func (client *TcpConnection) RemoteAddr() net.Addr {
  return client.conn.RemoteAddr()
}

func (client *TcpConnection) SetName(n string) {
  client.name = n
}

func (client *TcpConnection) String() string {
  return client.name
}

func (client *TcpConnection) Close() {
  client.closeOnce.Do(func() {
    close(client.closeConnChan)
    close(client.messageSendChan)
    close(client.handlerRecvChan)
    client.conn.Close()
    if (client.onClose != nil) {
      client.onClose(client)
    }
  })
}

func (client *TcpConnection) Write(msg Message) (err error) {
  select {
  case client.messageSendChan<- msg:
    return nil
  default:
    return ErrorWouldBlock
  }
}

func (client *TcpConnection) Do() {
  if client.onConnect != nil && !client.onConnect() {
    log.Fatalln("Error onConnect()\n")
  }

  // start read, write and handle loop
  client.startLoop(client.readLoop)
  client.startLoop(client.writeLoop)
  client.startLoop(client.handleLoop)
}

func (client *TcpConnection) startLoop(looper func()) {
  client.wg.Add(1)
  go func() {
    looper()
    client.wg.Done()
  }()
}

// use type-length-value format: |4 bytes|4 bytes|n bytes <= 8M|
// todo: maybe a special codec?
func (client *TcpConnection) readLoop() {
  defer func() {
    recover()
    client.Close()
  }()

  typeBytes := make([]byte, NTYPE)
  lengthBytes := make([]byte, NLEN)
  for {
    select {
    case <-client.closeConnChan:
      return

    default:
    }

    // read type info
    if _, err := client.conn.Read(typeBytes); err != nil {
      log.Println(err)
      return
    }
    typeBuf := bytes.NewReader(typeBytes)
    var msgType int32
    if err := binary.Read(typeBuf, binary.BigEndian, &msgType); err != nil {
      log.Fatalln(err)
    }

    // read length info
    if _, err := client.conn.Read(lengthBytes); err != nil {
      log.Println(err)
      return
    }
    lengthBuf := bytes.NewReader(lengthBytes)
    var msgLen uint32
    if err := binary.Read(lengthBuf, binary.BigEndian, &msgLen); err != nil {
      log.Fatalln(err)
    }
    if msgLen > MAXLEN {
      log.Printf("Error more than 8M data:%d\n", msgLen)
      return
    }

    // read real application message
    msgBytes := make([]byte, msgLen)
    if _, err := client.conn.Read(msgBytes); err != nil {
      log.Println(err)
      return
    }

    // deserialize message from bytes
    unmarshaler := MessageMap.get(msgType)
    if unmarshaler == nil {
      log.Printf("Error undefined message %d\n", msgType)
      continue
    }
    var msg Message
    var err error
    if msg, err = unmarshaler(msgBytes); err != nil {
      log.Printf("Error unmarshal message %d\n", msgType)
      continue
    }

    handlerFactory := HandlerMap.get(msgType)
    if handlerFactory == nil {
      log.Printf("message %d call onMessage()\n", msgType)
      client.onMessage(msg, client)
      continue
    }

    // send handler to handleLoop
    handler := handlerFactory(msg)
    client.handlerRecvChan<- handler
  }
}

func (client *TcpConnection) writeLoop() {
  defer func() {
    recover()
    client.Close()
  }()

  for {
    select {
    case <-client.closeConnChan:
      return

    case msg := <-client.messageSendChan:
      data, err := msg.MarshalBinary();
      if err != nil {
        log.Printf("Error serializing data\n")
        continue
      }
      buf := new(bytes.Buffer)
      binary.Write(buf, binary.BigEndian, msg.MessageNumber())
      binary.Write(buf, binary.BigEndian, int32(len(data)))
      binary.Write(buf, binary.BigEndian, data)
      packet := buf.Bytes()
      if _, err = client.conn.Write(packet); err != nil {
        log.Printf("Error writing data %s\n", err)
      }
    }
  }
}

func (client *TcpConnection) handleLoop() {
  defer func() {
    recover()
    client.Close()
  }()

  for {
    select {
    case <-client.closeConnChan:
      return

    case handler := <-client.handlerRecvChan:
      // todo: put handler into workers
      handler.Process(client)
    }
  }
}
