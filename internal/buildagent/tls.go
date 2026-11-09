package buildagent

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
)

func ServerTLSConfig(certPEM, keyPEM, clientCAPEM []byte) (*tls.Config, error) {
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	clientRoots := x509.NewCertPool()
	if !clientRoots.AppendCertsFromPEM(clientCAPEM) {
		return nil, errors.New("invalid build worker client CA")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientRoots}, nil
}

func WorkerTLSConfig(certPEM, keyPEM, serverCAPEM []byte, serverName string) (*tls.Config, error) {
	if serverName == "" {
		return nil, errors.New("build agent server name is required")
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	serverRoots := x509.NewCertPool()
	if !serverRoots.AppendCertsFromPEM(serverCAPEM) {
		return nil, errors.New("invalid build agent server CA")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate},
		RootCAs: serverRoots, ServerName: serverName}, nil
}
