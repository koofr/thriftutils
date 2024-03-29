/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package thriftutils

import (
	"context"
	"log"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/apache/thrift/lib/go/thrift"
)

type Server struct {
	closed int32
	wg     sync.WaitGroup
	mu     sync.Mutex

	processorFactory       thrift.TProcessorFactory
	serverTransport        thrift.TServerTransport
	inputTransportFactory  thrift.TTransportFactory
	outputTransportFactory thrift.TTransportFactory
	inputProtocolFactory   thrift.TProtocolFactory
	outputProtocolFactory  thrift.TProtocolFactory

	// Headers to auto forward in THeaderProtocol
	forwardHeaders []string

	connections map[thrift.TTransport]struct{}
}

func NewServer2(processor thrift.TProcessor, serverTransport thrift.TServerTransport) *Server {
	return NewServerFactory2(thrift.NewTProcessorFactory(processor), serverTransport)
}

func NewServer4(processor thrift.TProcessor, serverTransport thrift.TServerTransport, transportFactory thrift.TTransportFactory, protocolFactory thrift.TProtocolFactory) *Server {
	return NewServerFactory4(
		thrift.NewTProcessorFactory(processor),
		serverTransport,
		transportFactory,
		protocolFactory,
	)
}

func NewServer6(processor thrift.TProcessor, serverTransport thrift.TServerTransport, inputTransportFactory thrift.TTransportFactory, outputTransportFactory thrift.TTransportFactory, inputProtocolFactory thrift.TProtocolFactory, outputProtocolFactory thrift.TProtocolFactory) *Server {
	return NewServerFactory6(
		thrift.NewTProcessorFactory(processor),
		serverTransport,
		inputTransportFactory,
		outputTransportFactory,
		inputProtocolFactory,
		outputProtocolFactory,
	)
}

func NewServerFactory2(processorFactory thrift.TProcessorFactory, serverTransport thrift.TServerTransport) *Server {
	return NewServerFactory6(
		processorFactory,
		serverTransport,
		thrift.NewTTransportFactory(),
		thrift.NewTTransportFactory(),
		thrift.NewTBinaryProtocolFactoryDefault(),
		thrift.NewTBinaryProtocolFactoryDefault(),
	)
}

func NewServerFactory4(processorFactory thrift.TProcessorFactory, serverTransport thrift.TServerTransport, transportFactory thrift.TTransportFactory, protocolFactory thrift.TProtocolFactory) *Server {
	return NewServerFactory6(
		processorFactory,
		serverTransport,
		transportFactory,
		transportFactory,
		protocolFactory,
		protocolFactory,
	)
}

func NewServerFactory6(processorFactory thrift.TProcessorFactory, serverTransport thrift.TServerTransport, inputTransportFactory thrift.TTransportFactory, outputTransportFactory thrift.TTransportFactory, inputProtocolFactory thrift.TProtocolFactory, outputProtocolFactory thrift.TProtocolFactory) *Server {
	return &Server{
		processorFactory:       processorFactory,
		serverTransport:        serverTransport,
		inputTransportFactory:  inputTransportFactory,
		outputTransportFactory: outputTransportFactory,
		inputProtocolFactory:   inputProtocolFactory,
		outputProtocolFactory:  outputProtocolFactory,
		connections:            map[thrift.TTransport]struct{}{},
	}
}

func (p *Server) ProcessorFactory() thrift.TProcessorFactory {
	return p.processorFactory
}

func (p *Server) ServerTransport() thrift.TServerTransport {
	return p.serverTransport
}

func (p *Server) InputTransportFactory() thrift.TTransportFactory {
	return p.inputTransportFactory
}

func (p *Server) OutputTransportFactory() thrift.TTransportFactory {
	return p.outputTransportFactory
}

func (p *Server) InputProtocolFactory() thrift.TProtocolFactory {
	return p.inputProtocolFactory
}

func (p *Server) OutputProtocolFactory() thrift.TProtocolFactory {
	return p.outputProtocolFactory
}

func (p *Server) Listen() error {
	return p.serverTransport.Listen()
}

// SetForwardHeaders sets the list of header keys that will be auto forwarded
// while using THeaderProtocol.
//
// "forward" means that when the server is also a client to other upstream
// thrift servers, the context object user gets in the processor functions will
// have both read and write headers set, with write headers being forwarded.
// Users can always override the write headers by calling SetWriteHeaderList
// before calling thrift client functions.
func (p *Server) SetForwardHeaders(headers []string) {
	size := len(headers)
	if size == 0 {
		p.forwardHeaders = nil
		return
	}

	keys := make([]string, size)
	copy(keys, headers)
	p.forwardHeaders = keys
}

func (p *Server) innerAccept() (int32, error) {
	client, err := p.serverTransport.Accept()
	p.mu.Lock()
	defer p.mu.Unlock()
	closed := atomic.LoadInt32(&p.closed)
	if closed != 0 {
		if client != nil {
			client.Close()
		}
		return closed, nil
	}
	if err != nil {
		return 0, err
	}
	if client != nil {
		p.wg.Add(1)
		p.connections[client] = struct{}{}
		go func() {
			defer func() {
				p.mu.Lock()
				defer p.mu.Unlock()
				delete(p.connections, client)
				p.wg.Done()
			}()
			if err := p.processRequests(client); err != nil {
				log.Println("error processing request:", err)
			}
		}()
	}
	return 0, nil
}

func (p *Server) AcceptLoop() error {
	for {
		closed, err := p.innerAccept()
		if err != nil {
			return err
		}
		if closed != 0 {
			return nil
		}
	}
}

func (p *Server) Serve() error {
	err := p.Listen()
	if err != nil {
		return err
	}
	p.AcceptLoop()
	return nil
}

func (p *Server) Stop() error {
	p.mu.Lock()
	if atomic.LoadInt32(&p.closed) != 0 {
		p.mu.Unlock()
		return nil
	}
	atomic.StoreInt32(&p.closed, 1)
	p.serverTransport.Interrupt()
	for conn := range p.connections {
		conn.Close()
		delete(p.connections, conn)
	}
	p.mu.Unlock()
	p.wg.Wait()
	return nil
}

func (p *Server) processRequests(client thrift.TTransport) error {
	processor := p.processorFactory.GetProcessor(client)
	inputTransport, err := p.inputTransportFactory.GetTransport(client)
	if err != nil {
		return err
	}
	inputProtocol := p.inputProtocolFactory.GetProtocol(inputTransport)
	var outputTransport thrift.TTransport
	var outputProtocol thrift.TProtocol

	// for THeaderProtocol, we must use the same protocol instance for
	// input and output so that the response is in the same dialect that
	// the server detected the request was in.
	headerProtocol, ok := inputProtocol.(*thrift.THeaderProtocol)
	if ok {
		outputProtocol = inputProtocol
	} else {
		oTrans, err := p.outputTransportFactory.GetTransport(client)
		if err != nil {
			return err
		}
		outputTransport = oTrans
		outputProtocol = p.outputProtocolFactory.GetProtocol(outputTransport)
	}

	defer func() {
		if e := recover(); e != nil {
			log.Printf("panic in processor: %s: %s", e, debug.Stack())
		}
	}()

	if inputTransport != nil {
		defer inputTransport.Close()
	}
	if outputTransport != nil {
		defer outputTransport.Close()
	}
	for {
		if atomic.LoadInt32(&p.closed) != 0 {
			return nil
		}

		ctx := context.Background()
		if headerProtocol != nil {
			// We need to call ReadFrame here, otherwise we won't
			// get any headers on the AddReadTHeaderToContext call.
			//
			// ReadFrame is safe to be called multiple times so it
			// won't break when it's called again later when we
			// actually start to read the message.
			if err := headerProtocol.ReadFrame(); err != nil {
				return err
			}
			ctx = thrift.AddReadTHeaderToContext(ctx, headerProtocol.GetReadHeaders())
			ctx = thrift.SetWriteHeaderList(ctx, p.forwardHeaders)
		}

		ok, err := processor.Process(ctx, inputProtocol, outputProtocol)
		if err, ok := err.(thrift.TTransportException); ok && err.TypeId() == thrift.END_OF_FILE {
			return nil
		} else if err != nil {
			return err
		}
		if err, ok := err.(thrift.TApplicationException); ok && err.TypeId() == thrift.UNKNOWN_METHOD {
			continue
		}
		if !ok {
			break
		}
	}
	return nil
}
