// Command transportbench measures a raw encrypted backimage transport stream.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	backtransport "github.com/manprint/backimage/pkg/transport"
)

type result struct {
	Transport  string  `json:"transport"`
	Bytes      uint64  `json:"bytes"`
	Seconds    float64 `json:"seconds"`
	MiBPS      float64 `json:"mib_per_second"`
	CPUSeconds float64 `json:"cpu_seconds"`
}

func main() {
	var mode, transportName, address, certPath, keyPath, pin string
	var bytes uint64
	flag.StringVar(&mode, "mode", "", "server or client")
	flag.StringVar(&transportName, "transport", "", "tcp or quic")
	flag.StringVar(&address, "address", "127.0.0.1:7590", "listen or connect address")
	flag.StringVar(&certPath, "cert", "", "PEM server certificate")
	flag.StringVar(&keyPath, "key", "", "PEM server private key")
	flag.StringVar(&pin, "pin", "", "SHA-256 server certificate fingerprint")
	flag.Uint64Var(&bytes, "bytes", 4<<30, "bytes sent by a client")
	flag.Parse()

	if err := run(mode, transportName, address, certPath, keyPath, pin, bytes); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(mode, transportName, address, certPath, keyPath, pin string, bytes uint64) error {
	if transportName != "tcp" && transportName != "quic" {
		return errors.New("--transport must be tcp or quic")
	}
	switch mode {
	case "server":
		return serve(transportName, address, certPath, keyPath)
	case "client":
		return send(transportName, address, pin, bytes)
	default:
		return errors.New("--mode must be server or client")
	}
}

func serve(transportName, address, certPath, keyPath string) error {
	if certPath == "" || keyPath == "" {
		return errors.New("--cert and --key are required in server mode")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("load certificate: %w", err)
	}
	listener, err := backtransport.NewListener(transportName, address, backtransport.Config{TLS: &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert},
	}})
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	fmt.Printf("READY %s\n", listener.Addr())
	stream, err := listener.Accept(context.Background())
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	defer stream.Close()
	start, cpuStart := time.Now(), processCPU()
	n, err := io.Copy(io.Discard, stream)
	if err != nil {
		return fmt.Errorf("discard stream: %w", err)
	}
	if _, err := stream.Write([]byte{1}); err != nil {
		return fmt.Errorf("acknowledge stream: %w", err)
	}
	printResult(resultFor(transportName, uint64(n), time.Since(start), processCPU()-cpuStart))
	return nil
}

func send(transportName, address, pin string, bytes uint64) error {
	if pin == "" {
		return errors.New("--pin is required in client mode")
	}
	tlsConfig, err := backtransport.PinnedClientTLS(pin, nil)
	if err != nil {
		return err
	}
	dialer, err := backtransport.NewDialer(transportName, backtransport.Config{TLS: tlsConfig})
	if err != nil {
		return err
	}
	stream, err := dialer.Dial(context.Background(), address)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	buf := make([]byte, 1<<20)
	start, cpuStart := time.Now(), processCPU()
	var written uint64
	for written < bytes {
		chunk := buf
		if remain := bytes - written; remain < uint64(len(chunk)) {
			chunk = chunk[:remain]
		}
		n, err := stream.Write(chunk)
		written += uint64(n)
		if err != nil {
			return errors.Join(fmt.Errorf("write stream: %w", err), closeClientStream(stream))
		}
		if n != len(chunk) {
			return errors.Join(io.ErrShortWrite, closeClientStream(stream))
		}
	}
	if err := stream.Close(); err != nil {
		return fmt.Errorf("finish stream: %w", err)
	}
	var ack [1]byte
	if _, err := io.ReadFull(stream, ack[:]); err != nil {
		return fmt.Errorf("read server acknowledgement: %w", err)
	}
	if closer, ok := stream.(backtransport.ConnectionCloser); ok {
		if err := closer.CloseConnection(); err != nil {
			return fmt.Errorf("close QUIC connection: %w", err)
		}
	}
	printResult(resultFor(transportName, written, time.Since(start), processCPU()-cpuStart))
	return nil
}

func closeClientStream(stream backtransport.Stream) error {
	streamErr := stream.Close()
	if closer, ok := stream.(backtransport.ConnectionCloser); ok {
		return errors.Join(streamErr, closer.CloseConnection())
	}
	return streamErr
}

func resultFor(transportName string, bytes uint64, elapsed, cpu time.Duration) result {
	seconds := elapsed.Seconds()
	return result{
		Transport: transportName, Bytes: bytes, Seconds: seconds,
		MiBPS: float64(bytes) / float64(1<<20) / seconds, CPUSeconds: cpu.Seconds(),
	}
}

func printResult(v result) {
	if err := json.NewEncoder(os.Stdout).Encode(v); err != nil {
		panic(err)
	}
}

func processCPU() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Sec+usage.Stime.Sec)*time.Second +
		time.Duration(usage.Utime.Usec+usage.Stime.Usec)*time.Microsecond
}
