package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"
)

type Query struct {
	Name string
	Type uint16
}

func main() {
	os.Exit(main2())
}

func main2() int {
	var (
		server    string
		dnssec    bool
		recursion bool
		keysPath  string
		queries   []Query
		timeout   time.Duration
		tlsCA     string
		tlsCert   string
		tlsKey    string
	)

	flag.Usage = func() {
		fmt.Printf("usage: %s <options> (<qname> <qtype>)...\n\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.StringVar(&server, "server", "127.0.0.1:853", "DNS-over-QUIC server to use.")
	flag.BoolVar(&dnssec, "dnssec", true, "Send DNSSEC OK flag.")
	flag.BoolVar(&recursion, "recursion", true, "Send RD flag.")
	flag.StringVar(&keysPath, "export_keys_path", "", "File name to export session keys for decryption.")
	flag.DurationVar(&timeout, "timeout", 3*time.Second, "Connection timeout.")
	flag.StringVar(&tlsCA, "ca_certs", "", "Path to CA certificate bundle.")
	flag.StringVar(&tlsCert, "cert", "", "Path to client TLS certificate.")
	flag.StringVar(&tlsKey, "key", "", "Path to client TLS key.")
	flag.Parse()

	if flag.NArg() == 0 || flag.NArg()%2 != 0 {
		flag.Usage()
		return 1
	}

	for i := 0; (i + 1) < flag.NArg(); i += 2 {
		qname := dns.Fqdn(flag.Arg(i))
		qtype, ok := dns.StringToType[flag.Arg(i+1)]
		if !ok {
			fmt.Fprintf(os.Stderr, "invalid qtype: %s\n", flag.Arg(i+1))
			return 1
		}
		if qtype == dns.TypeIXFR {
			// TODO: Allow user to pass in serial number for IXFR
			fmt.Fprintf(os.Stderr, "skipping unsupported qtype: %s\n", flag.Arg(i+1))
		} else {
			queries = append(queries, Query{qname, qtype})
		}
	}

	var keyLog io.Writer
	if keysPath != "" {
		w, err := os.OpenFile(keysPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open file for session keys: %s\n", err)
			return 1
		}
		defer w.Close()
		keyLog = w
	}

	tlsConfig := tls.Config{
		NextProtos:   []string{"doq"},
		KeyLogWriter: keyLog,
	}

	// server certificate validation
	if tlsCA == "" {
		tlsConfig.InsecureSkipVerify = true
	} else {
		data, err := os.ReadFile(tlsCA)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to load CA certificate bundle: %s\n", err)
			return 1
		}
		pool := x509.NewCertPool()
		if ok := pool.AppendCertsFromPEM(data); !ok {
			fmt.Fprintf(os.Stderr, "failed to load CA certificate bundle: no certificate found\n")
			return 1
		}
		tlsConfig.RootCAs = pool
	}

	// client certificate
	if tlsCert != "" && tlsKey != "" {
		cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to load TLS certificate: %s\n", err)
			return 1
		}

		tlsConfig.Certificates = append(tlsConfig.Certificates, cert)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	session, err := quic.DialAddr(ctx, server, &tlsConfig, nil)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect to the server: %s\n", err)
		return 1
	}
	defer session.CloseWithError(0, "")

	print := make(chan string)

	wg := sync.WaitGroup{}
	wg.Add(len(queries))

	for _, query := range queries {
		go func(query Query) {
			err := SendQuery(session, &query, dnssec, recursion, print)
			if err != nil {
				print <- fmt.Sprintf("failed to send query: %s\n", err)
			}
			wg.Done()
		}(query)
	}

	go func() {
		wg.Wait()
		close(print)
	}()

	for p := range print {
		fmt.Println(p)
	}

	return 0
}

func SendQuery(session *quic.Conn, query *Query, dnssec, recursion bool, print chan (string)) error {
	stream, err := session.OpenStream()
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	msg := dns.Msg{}
	msg.SetQuestion(query.Name, query.Type)
	msg.RecursionDesired = recursion
	msg.SetEdns0(4096, dnssec)
	msg.Id = 0
	wire, err := msg.Pack()
	if err != nil {
		stream.Close()
		return fmt.Errorf("pack query: %w", err)
	}

	bundle := make([]byte, 2+len(wire))
	binary.BigEndian.PutUint16(bundle, uint16(len(wire)))
	copy(bundle[2:], wire)
	_, err = stream.Write(bundle)
	stream.Close()
	if err != nil {
		return fmt.Errorf("send query: %w", err)
	}

	stream.SetDeadline(time.Now().Add(1 * time.Second))

	for {
		var length uint16
		if err := binary.Read(stream, binary.BigEndian, &length); err != nil {
			// Ignore timeout related errors as this is how we close this connection for now
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("read response length: %w", err)
		}

		buf := make([]byte, length)
		_, err := io.ReadFull(stream, buf)
		if err != nil {
			return fmt.Errorf("read response payload: %w", err)
		}

		resp := dns.Msg{}
		err = resp.Unpack(buf)
		if err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		print <- resp.String()
		switch msg.Question[0].Qtype {
		case dns.TypeAXFR, dns.TypeIXFR:
		default:
			return nil
		}
	}
}
