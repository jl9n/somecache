package master

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/756445638/somecache/common"
	"github.com/756445638/somecache/message"
)

type V1Slave struct {
	stoped    bool
	t         int64
	lock      sync.Mutex
	conn      net.Conn
	reader    *bufio.Reader
	ctx       context
	slave     *Slave
	closechan chan struct{}
	jobschan  chan *job
	pingpool  sync.Pool
}

type job struct {
	c         *common.Command
	diff      interface{} //diff is a field can receive any kind of data
	errorchan chan error  //errorchan is also an endchanv that will notify caller to exit
}

func (v1s *V1Slave) close() {
	v1s.lock.Lock()
	defer v1s.lock.Unlock()
	v1s.stoped = true
	v1s.conn.Close()
	close(v1s.closechan)
	close(v1s.jobschan)
}

func (v1s *V1Slave) Login(c chan bool) error {
	line, err := common.ReadLine(v1s.reader)
	if err != nil {
		return err
	}
	if !bytes.Equal(line, common.COMMAND_LOGIN) {
		return fmt.Errorf("first package must be login")
	}
	body, _, err := common.ReadBody4(v1s.reader, nil)
	if err != nil {
		return err
	}
	login := &message.Login{}
	err = json.Unmarshal(body, login)
	if err != nil {
		return err
	}
	v1s.slave.loginmessage = login
	_, err = common.NewCommand(common.OK, nil, nil).Write(v1s.conn)
	if err != nil {
		return err
	}
	return nil
}

//main loop
func (v1s *V1Slave) MainLoop(conn net.Conn, c chan bool) {
	v1s.conn = conn
	defer v1s.close()
	v1s.reader = bufio.NewReader(v1s.conn)
	err := v1s.Login(c)
	c <- (err == nil)
	if err != nil {
		fmt.Println("login failed,err:", err)
		return
	}
	pingticker := time.NewTicker(time.Second)
	for {
		select {
		case <-pingticker.C:
			v1s.t = time.Now().UnixNano()
			err := v1s.Ping()
			v1s.t = -1
			if err != nil {
				fmt.Println("ping failed,err:", err)
				return
			}
		case d := <-v1s.jobschan:
			v1s.t = time.Now().UnixNano()
			v1s.exec(d)
			v1s.t = -1
		case <-v1s.closechan:
			goto exit
		}
	}
exit:
}

func (v1s *V1Slave) exec(d *job) { //dispath method
	var e error
	if bytes.Equal(d.c.Command, common.COMMAND_GET) {
		e = v1s.get(d)
	} else if bytes.Equal(d.c.Command, common.COMMAND_GET_STREAM) {
		e = v1s.get_stream(d)
	} else if bytes.Equal(d.c.Command, common.COMMAND_PUT) {
		e = v1s.put(d)
	} else if bytes.Equal(d.c.Command, common.COMMAND_PUT_FROM_READER) {
		e = v1s.put_from_reader(d)
	} else {
		e = fmt.Errorf("no such command")
	}
	d.errorchan <- e
}
func (v1s *V1Slave) put_from_reader(d *job) error {
	err := v1s.writeCommandAndReadOk(d)
	if err != nil {
		return err
	}
	reader := d.diff.(io.Reader)
	_, err = io.Copy(v1s.conn, reader)
	return err
}

func (v1s *V1Slave) writeCommandAndReadOk(d *job) error {
	_, err := d.c.Write(v1s.conn)
	if err != nil {
		return err
	}
	line, err := common.ReadLine(v1s.reader)
	if err != nil {
		return err
	}
	if !bytes.Equal(line, common.OK) {
		return fmt.Errorf(string(line))
	}
	return nil
}

func (v1s *V1Slave) get(d *job) error {
	err := v1s.writeCommandAndReadOk(d)
	if err != nil {
		return err
	}
	dest := d.diff.(*[]byte)
	*dest, _, err = common.ReadBody4(v1s.reader, nil)
	return err
}

func (v1s *V1Slave) get_stream(d *job) error {
	err := v1s.writeCommandAndReadOk(d)
	if err != nil {
		return err
	}
	dest := d.diff.(io.Writer)
	_, _, err = common.ReadBody4(v1s.reader, dest)
	return err
}

func (v1s *V1Slave) put(d *job) error {
	err := v1s.writeCommandAndReadOk(d)
	if err != nil {
		return err
	}
	return nil
}

//ping is  hearbeat
func (v1s *V1Slave) Ping() error {
	v1s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := common.NewCommand(common.COMMAND_PING, nil, nil).Write(v1s.conn)
	if err != nil {
		return err
	}
	v1s.conn.SetReadDeadline(time.Now().Add(readTimeout))
	res, err := common.ReadLine(v1s.reader)
	if err != nil {
		return err
	}
	if !bytes.Equal(res, common.OK) {
		return fmt.Errorf("slave send response but not OK")
	}

	buf, _, err := common.ReadBody4(v1s.reader, nil)
	if err != nil {
		return err
	}
	v := v1s.pingpool.Get()
	if v == nil {
		v = &message.HeartBeat{}
	}
	defer v1s.pingpool.Put(v)
	m := v.(*message.HeartBeat)
	err = json.Unmarshal(buf, m)
	if err != nil {
		return err
	}
	fmt.Printf(
		"slave[%s] process ping ok,details are "+
			"hit[%d]"+
			"cachedsize[%d]"+
			"gets[%d]"+
			"puts[%d]"+
			"maxcachesize[%d]\n",

		v1s.slave.addr.String(),
		m.Lru_hit,
		m.Lru_cachedsize,
		m.Lru_gets,
		m.Lru_puts,
		m.Lru_maxcachesize,
	)
	return nil
}

func (v1s *V1Slave) Close() {
	v1s.closechan <- struct{}{}
}

//-1 means not busy,positive numbre menas how long I have been busy
func (v1s *V1Slave) IfBusy() int64 {
	if v1s.t == -1 {
		return -1
	}
	return time.Now().UnixNano() - v1s.t
}

func (v1s *V1Slave) Get(key string) ([]byte, error) {
	v1s.lock.Lock()
	defer v1s.lock.Unlock()
	if v1s.stoped {
		return nil, stopped
	}
	var b []byte
	errorchan := make(chan error)
	v1s.jobschan <- &job{
		c:         common.NewCommand(common.COMMAND_GET, [][]byte{[]byte(key)}, nil),
		diff:      &b,
		errorchan: errorchan,
	}
	var e error
	select {
	case e = <-errorchan:
	}
	return b, e
}

func (v1s *V1Slave) Put(key string, data []byte) error {
	v1s.lock.Lock()
	defer v1s.lock.Unlock()
	if v1s.stoped {
		return stopped
	}
	errorchan := make(chan error)
	v1s.jobschan <- &job{
		c:         common.NewCommand(common.COMMAND_PUT, [][]byte{[]byte(key)}, data),
		diff:      nil,
		errorchan: errorchan,
	}
	var e error
	select {
	case e = <-errorchan:
	}
	return e
}

func (v1s *V1Slave) Get2Stream(key string, w io.Writer) error {
	v1s.lock.Lock()
	defer v1s.lock.Unlock()
	if v1s.stoped {
		return stopped
	}
	errorchan := make(chan error)
	v1s.jobschan <- &job{
		c:         common.NewCommand(common.COMMAND_GET_STREAM, [][]byte{[]byte(key)}, nil),
		diff:      w,
		errorchan: errorchan,
	}
	var e error
	select {
	case e = <-errorchan:
	}
	return e
}

var stopped = errors.New("slave stoped")

func (v1s *V1Slave) PutFromReader(key string, reader io.Reader) error {
	v1s.lock.Lock()
	defer v1s.lock.Unlock()
	if v1s.stoped {
		return stopped
	}
	errorchan := make(chan error)
	v1s.jobschan <- &job{
		c:         common.NewCommand(common.COMMAND_PUT_FROM_READER, [][]byte{[]byte(key)}, nil),
		diff:      reader,
		errorchan: errorchan,
	}
	var e error
	select {
	case e = <-errorchan:
	}
	return e
}
