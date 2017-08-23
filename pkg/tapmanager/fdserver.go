/*
Copyright 2017 Mirantis

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tapmanager

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/golang/glog"
)

const (
	minAcceptErrorDelay = 5 * time.Millisecond
	maxAcceptErrorDelay = 1 * time.Second
	receiveFdTimeout    = 5 * time.Second
	fdMagic             = 0x42424242
	fdAdd               = 0
	fdRelease           = 1
	fdGet               = 2
	fdResponse          = 0x80
	fdAddResponse       = fdAdd | fdResponse
	fdReleaseResponse   = fdRelease | fdResponse
	fdGetResponse       = fdGet | fdResponse
	fdError             = 0xff
)

type FDManager interface {
	AddFD(key string, data interface{}) ([]byte, error)
	ReleaseFD(key string) error
}

type fdHeader struct {
	Magic    uint32
	Command  uint8
	DataSize uint32
	OobSize  uint32
	Key      [64]byte
}

func (hdr *fdHeader) GetKey() string {
	return strings.TrimSpace(string(hdr.Key[:]))
}

func fdKey(key string) [64]byte {
	var r [64]byte
	for n := range r {
		if n < len(key) {
			r[n] = key[n]
		} else {
			r[n] = 32
		}
	}
	return r
}

type FDSource interface {
	GetFD(key string, data []byte) (int, []byte, error)
	Release(key string) error
	GetInfo(key string) ([]byte, error)
}

type FDServer struct {
	sync.Mutex
	l          *net.UnixListener
	socketPath string
	source     FDSource
	fds        map[string]int
	stopCh     chan struct{}
}

func NewFDServer(socketPath string, source FDSource) *FDServer {
	return &FDServer{
		socketPath: socketPath,
		source:     source,
		fds:        make(map[string]int),
	}
}

func (s *FDServer) addFD(key string, fd int) bool {
	s.Lock()
	defer s.Unlock()
	if _, found := s.fds[key]; found {
		return false
	}
	s.fds[key] = fd
	return true
}

func (s *FDServer) removeFD(key string) {
	s.Lock()
	defer s.Unlock()
	delete(s.fds, key)
}

func (s *FDServer) getFD(key string) (int, error) {
	s.Lock()
	defer s.Unlock()
	fd, found := s.fds[key]
	if !found {
		return 0, fmt.Errorf("bad fd key: %q", key)
	}
	return fd, nil
}

func (s *FDServer) Serve() error {
	s.Lock()
	defer s.Unlock()
	if s.stopCh != nil {
		return errors.New("already listening")
	}
	addr, err := net.ResolveUnixAddr("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("failed to resolve unix addr %q: %v", s.socketPath, err)
	}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		l.Close()
		return fmt.Errorf("failed to listen on socket %q: %v", s.socketPath, err)
	}
	// Accept error handling is inspired by server.go in grpc
	s.stopCh = make(chan struct{})
	var delay time.Duration
	go func() {
		for {
			conn, err := l.AcceptUnix()
			if err != nil {
				if temp, ok := err.(interface {
					Temporary() bool
				}); ok && temp.Temporary() {
					glog.Warningf("Accept error: %v", err)
					if delay == 0 {
						delay = minAcceptErrorDelay
					} else {
						delay *= 2
					}
					if delay > maxAcceptErrorDelay {
						delay = maxAcceptErrorDelay
					}
					select {
					case <-time.After(delay):
						continue
					case <-s.stopCh:
						return
					}
				}
				select {
				case <-s.stopCh:
					// this error is expected
					return
				default:
				}
				glog.Errorf("Accept failed: %v", err)
				break
			}
			go func() {
				err := s.serveConn(conn)
				if err != nil {
					glog.Error(err)
				}
			}()
		}
	}()
	return nil
}

func (s *FDServer) serveAdd(c *net.UnixConn, hdr *fdHeader) (*fdHeader, []byte, error) {
	data := make([]byte, hdr.DataSize)
	if len(data) > 0 {
		if _, err := io.ReadFull(c, data); err != nil {
			return nil, nil, fmt.Errorf("error reading payload: %v", err)
		}
	}
	key := hdr.GetKey()
	fd, respData, err := s.source.GetFD(key, data)
	if err != nil {
		return nil, nil, fmt.Errorf("error getting fd: %v", err)
	}
	if !s.addFD(key, fd) {
		return nil, nil, fmt.Errorf("fd key already exists: %q", err)
	}
	return &fdHeader{
		Magic:    fdMagic,
		Command:  fdAddResponse,
		DataSize: uint32(len(respData)),
		Key:      hdr.Key,
	}, respData, nil
}

func (s *FDServer) serveRelease(c *net.UnixConn, hdr *fdHeader) (*fdHeader, error) {
	s.removeFD(hdr.GetKey())
	return &fdHeader{
		Magic:   fdMagic,
		Command: fdReleaseResponse,
		Key:     hdr.Key,
	}, nil
}

func (s *FDServer) serveGet(c *net.UnixConn, hdr *fdHeader) (*fdHeader, []byte, []byte, error) {
	key := hdr.GetKey()
	fd, err := s.getFD(key)
	if err != nil {
		return nil, nil, nil, err
	}
	info, err := s.source.GetInfo(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("can't get key info: %v", err)
	}
	rights := syscall.UnixRights(fd)
	return &fdHeader{
		Magic:    fdMagic,
		Command:  fdGetResponse,
		DataSize: uint32(len(info)),
		OobSize:  uint32(len(rights)),
		Key:      hdr.Key,
	}, info, rights, nil
}

func (s *FDServer) serveConn(c *net.UnixConn) error {
	defer c.Close()
	for {
		var hdr fdHeader
		if err := binary.Read(c, binary.BigEndian, &hdr); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("error reading the header: %v", err)
		}
		if hdr.Magic != fdMagic {
			return errors.New("bad magic")
		}

		var err error
		var respHdr *fdHeader
		var data, oobData []byte
		switch hdr.Command {
		case fdAdd:
			respHdr, data, err = s.serveAdd(c, &hdr)
		case fdRelease:
			respHdr, err = s.serveRelease(c, &hdr)
		case fdGet:
			respHdr, data, oobData, err = s.serveGet(c, &hdr)
		default:
			err = errors.New("bad command")
		}

		if err != nil {
			data = []byte(err.Error())
			oobData = nil
			respHdr = &fdHeader{
				Magic:    fdMagic,
				Command:  fdError,
				DataSize: uint32(len(data)),
				OobSize:  0,
			}
		}

		if err := binary.Write(c, binary.BigEndian, respHdr); err != nil {
			return fmt.Errorf("error writing response header: %v", err)
		}
		if len(data) > 0 || len(oobData) > 0 {
			if data == nil {
				data = []byte{}
			}
			if oobData == nil {
				oobData = []byte{}
			}
			if _, _, err = c.WriteMsgUnix(data, oobData, nil); err != nil {
				return fmt.Errorf("error writing payload: %v", err)
			}
		}
		// } else if len(data) > 0 {
		// 	err = binary.Write(c, binary.BigEndian, data)
		// }
	}
	return nil
}

func (s *FDServer) Stop() {
	s.Lock()
	defer s.Unlock()
	if s.stopCh != nil {
		close(s.stopCh)
		s.l.Close()
		s.stopCh = nil
	}
}

type FDClient struct {
	socketPath string
	c          *net.UnixConn
}

var _ FDManager = &FDClient{}

func NewFDClient(socketPath string) *FDClient {
	return &FDClient{socketPath: socketPath}
}

func (c *FDClient) Connect() error {
	if c.c != nil {
		return nil
	}

	addr, err := net.ResolveUnixAddr("unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("failed to resolve unix addr %q: %v", c.socketPath, err)
	}

	conn, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		return fmt.Errorf("can't connect to %q: %v", c.socketPath, err)
	}
	c.c = conn
	return nil
}

func (c *FDClient) Close() error {
	var err error
	if c.c != nil {
		err = c.c.Close()
		c.c = nil
	}
	return err
}

func (c *FDClient) request(hdr *fdHeader, data []byte) (*fdHeader, []byte, []byte, error) {
	hdr.Magic = fdMagic
	if c.c == nil {
		return nil, nil, nil, errors.New("not connected")
	}

	if err := binary.Write(c.c, binary.BigEndian, hdr); err != nil {
		return nil, nil, nil, fmt.Errorf("error writing request header: %v", err)
	}

	if len(data) > 0 {
		if err := binary.Write(c.c, binary.BigEndian, data); err != nil {
			return nil, nil, nil, fmt.Errorf("error writing request payload: %v", err)
		}
	}

	var respHdr fdHeader
	if err := binary.Read(c.c, binary.BigEndian, &respHdr); err != nil {
		return nil, nil, nil, fmt.Errorf("error reading response header: %v", err)
	}
	if respHdr.Magic != fdMagic {
		return nil, nil, nil, errors.New("bad magic")
	}

	respData := make([]byte, respHdr.DataSize)
	oobData := make([]byte, respHdr.OobSize)
	if len(respData) > 0 || len(oobData) > 0 {
		n, oobn, _, _, err := c.c.ReadMsgUnix(respData, oobData)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("error reading the message: %v", err)
		}
		// ReadMsgUnix will read & discard a single byte if len(respData) == 0
		if n != len(respData) && (len(respData) != 0 || n != 1) {
			return nil, nil, nil, fmt.Errorf("bad data size: %d instead of %d", n, len(respData))
		}
		if oobn != len(oobData) {
			return nil, nil, nil, fmt.Errorf("bad oob data size: %d instead of %d", oobn, len(oobData))
		}
	}

	if respHdr.Command == fdError {
		return nil, nil, nil, fmt.Errorf("server returned error: %s", respData)
	}

	if respHdr.Command != hdr.Command|fdResponse {
		return nil, nil, nil, fmt.Errorf("unexpected command %02x", respHdr.Command)
	}

	return &respHdr, respData, oobData, nil
}

func (c *FDClient) AddFD(key string, data interface{}) ([]byte, error) {
	bs, ok := data.([]byte)
	if !ok {
		var err error
		bs, err = json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("error marshalling json: %v", err)
		}
	}
	respHdr, respData, _, err := c.request(&fdHeader{
		Command:  fdAdd,
		DataSize: uint32(len(bs)),
		Key:      fdKey(key),
	}, bs)
	if err != nil {
		return nil, err
	}
	if respHdr.GetKey() != key {
		return nil, fmt.Errorf("fd key mismatch in the server response")
	}
	return respData, nil
}

func (c *FDClient) ReleaseFD(key string) error {
	_, _, _, err := c.request(&fdHeader{
		Command: fdRelease,
		Key:     fdKey(key),
	}, nil)
	return err
}

func (c *FDClient) GetFD(key string) (int, []byte, error) {
	_, respData, oobData, err := c.request(&fdHeader{
		Command: fdGet,
		Key:     fdKey(key),
	}, nil)
	if err != nil {
		return 0, nil, err
	}

	scms, err := syscall.ParseSocketControlMessage(oobData)
	if err != nil {
		return 0, nil, fmt.Errorf("couldn't parse socket control message: %v", err)
	}
	if len(scms) != 1 {
		return 0, nil, fmt.Errorf("unexpected number of socket control messages: %d instead of 1", len(scms))
	}

	fds, err := syscall.ParseUnixRights(&scms[0])
	if err != nil {
		return 0, nil, fmt.Errorf("can't decode file descriptors: %v", err)
	}
	if len(fds) != 1 {
		return 0, nil, fmt.Errorf("unexpected number of file descriptors: %d instead of 1", len(fds))
	}
	return fds[0], respData, nil
}
