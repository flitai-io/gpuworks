// Package kubeapply applies a cluster's join manifest (kubemanifest) to a
// Kubernetes API server, or removes it, with a kubeconfig the cloud issued
// for that one operation (ADR 0063). It is a leaf: kubeconfig and manifest
// in, per-object results out; it keeps nothing.
package kubeapply

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// kubeconfig is the part of a kubeconfig file this package reads.
type kubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			CertificateAuthority     string `yaml:"certificate-authority"`
			InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			ClientCertificateData string         `yaml:"client-certificate-data"`
			ClientKeyData         string         `yaml:"client-key-data"`
			ClientCertificate     string         `yaml:"client-certificate"`
			Token                 string         `yaml:"token"`
			Exec                  map[string]any `yaml:"exec"`
			AuthProvider          map[string]any `yaml:"auth-provider"`
		} `yaml:"user"`
	} `yaml:"users"`
}

// Client talks to one API server as one kubeconfig user.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// Server is the API server address the client talks to.
func (c *Client) Server() string { return c.base.String() }

// NewClient reads a kubeconfig's current context. Only credentials carried
// in the file are accepted: a kubeconfig that runs a program (exec, an auth
// provider) or names files on some other machine cannot be used here.
func NewClient(data []byte) (*Client, error) {
	var kc kubeconfig
	if err := yaml.Unmarshal(data, &kc); err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	ctxName := kc.CurrentContext
	if ctxName == "" && len(kc.Contexts) == 1 {
		ctxName = kc.Contexts[0].Name
	}
	var clusterName, userName string
	for _, c := range kc.Contexts {
		if c.Name == ctxName {
			clusterName, userName = c.Context.Cluster, c.Context.User
		}
	}
	if clusterName == "" {
		return nil, fmt.Errorf("kubeconfig: no context %q", ctxName)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	var server string
	found := false
	for _, c := range kc.Clusters {
		if c.Name != clusterName {
			continue
		}
		found = true
		server = c.Cluster.Server
		switch {
		case c.Cluster.CertificateAuthority != "":
			return nil, errors.New("kubeconfig: the cluster's CA is a file path, not data")
		case c.Cluster.CertificateAuthorityData != "":
			pem, err := base64.StdEncoding.DecodeString(c.Cluster.CertificateAuthorityData)
			if err != nil {
				return nil, fmt.Errorf("kubeconfig: certificate-authority-data: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("kubeconfig: certificate-authority-data holds no certificate")
			}
			tlsConfig.RootCAs = pool
		case c.Cluster.InsecureSkipTLSVerify:
			tlsConfig.InsecureSkipVerify = true //nolint:gosec // the kubeconfig says so
		}
	}
	if !found {
		return nil, fmt.Errorf("kubeconfig: no cluster %q", clusterName)
	}
	base, err := url.Parse(strings.TrimRight(server, "/"))
	if err != nil || base.Host == "" || base.Scheme != "https" {
		return nil, fmt.Errorf("kubeconfig: server %q is not an https address", server)
	}
	c := &Client{base: base}
	found = false
	for _, u := range kc.Users {
		if u.Name != userName {
			continue
		}
		found = true
		switch {
		case u.User.Exec != nil || u.User.AuthProvider != nil:
			return nil, errors.New("kubeconfig: the user's credentials come from a program (exec or auth-provider), which cannot run here")
		case u.User.ClientCertificate != "":
			return nil, errors.New("kubeconfig: the user's certificate is a file path, not data")
		case u.User.ClientCertificateData != "":
			cert, err := base64.StdEncoding.DecodeString(u.User.ClientCertificateData)
			if err != nil {
				return nil, fmt.Errorf("kubeconfig: client-certificate-data: %w", err)
			}
			key, err := base64.StdEncoding.DecodeString(u.User.ClientKeyData)
			if err != nil {
				return nil, fmt.Errorf("kubeconfig: client-key-data: %w", err)
			}
			pair, err := tls.X509KeyPair(cert, key)
			if err != nil {
				return nil, fmt.Errorf("kubeconfig: client certificate: %w", err)
			}
			tlsConfig.Certificates = []tls.Certificate{pair}
		case u.User.Token != "":
			c.token = u.User.Token
		default:
			return nil, errors.New("kubeconfig: the user has no credentials")
		}
	}
	if !found {
		return nil, fmt.Errorf("kubeconfig: no user %q", userName)
	}
	c.http = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig, Proxy: http.ProxyFromEnvironment}}
	return c, nil
}
