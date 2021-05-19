// Copyright (C) 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/imagetools"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (d *Driver) GetAuthProvider(secretName string, stderr io.Writer) session.Attachable {
	if secretName == "" {
		secretName = buildxNameToDeploymentName(d.InitConfig.Name)
	}
	return &authProvider{
		driver: d,
		name:   secretName,
	}
}

func (d *Driver) GetAuthHintMessage() string {
	return d.authHintMessage
}

type authProvider struct {
	driver *Driver
	name   string
	secret *corev1.Secret
	// softFailure bool
}

// TODO unwind this abstraction...
func (ap *authProvider) GetAuthConfig(registryHostname string) (imagetools.AuthConfig, error) {
	res := imagetools.AuthConfig{}
	credResponse, err := ap.Credentials(context.Background(), &auth.CredentialsRequest{Host: registryHostname})
	if err != nil {
		return res, err
	}
	res.Username = credResponse.Username
	res.Password = credResponse.Secret

	return res, nil
}

func (ap *authProvider) Register(server *grpc.Server) {
	auth.RegisterAuthServer(server, ap)
}

type creds struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	Auth          string `json:"auth"`
	IdentityToken string `json:"identitytoken"`
}
type credStore struct {
	Auths map[string]creds `json:"auths"`
}

func (ap *authProvider) Credentials(ctx context.Context, req *auth.CredentialsRequest) (*auth.CredentialsResponse, error) {
	registries := credStore{}
	res := &auth.CredentialsResponse{}
	if req.Host == "registry-1.docker.io" {
		req.Host = "https://index.docker.io/v1/"
	}
	if ap.secret == nil {
		// For secret lookup calls, we don't hard-fail, but record a hint message in case the entire build fails
		// This avoids causing problems for local builds (non-push) based on public images (allowing anonymous operation)
		secret, err := ap.driver.secretClient.Get(ctx, ap.name, metav1.GetOptions{})
		if err != nil {
			if kubeerrors.IsNotFound(err) {
				ap.driver.authHintMessage = fmt.Sprintf("unable to find secret \"%s\" - if you used a different name specify with --registry-secret - if you haven't created a secret yet follow these instructions https://kubernetes.io/docs/tasks/configure-pod-container/pull-image-private-registry/", ap.name)
				return res, nil
			}
			ap.driver.authHintMessage = fmt.Sprintf("failed to lookup secret \"%s\": %s", ap.name, err)
			return res, nil
		}
		ap.secret = secret
	}

	// Make sure the secret is a properly formatted registry secret
	data, ok := ap.secret.Data[".dockerconfigjson"]
	if !ok {
		return nil, fmt.Errorf("malformed kubernetes registry secret - missing '.dockerconfigjson' data key")
	}
	err := json.Unmarshal(data, &registries)
	if err != nil {
		return nil, fmt.Errorf("malformed kubernetes registry secret - '.dockerconfigjson' didn't contain valid cred store: %w", err)
	}

	creds, found := registries.Auths[req.Host] // TODO this code needs work...
	if found {
		if (creds.Username == "" || creds.Password == "") && creds.Auth != "" {
			creds.Username, creds.Password, err = decodeAuth(creds.Auth)
			if err != nil {
				return nil, fmt.Errorf("malformed kubernetes registry secret - failed to decode auth %w", err)
			}
		}
	} else { // TODO remove this extra debugging once things are sorted out...
		// Can we breadcrumb the potential failure here so that if we see the build fail we can give
		// a more helpful error message to the user?
		logrus.Infof("no credentials found for registry %s (proceeding with anonymous auth)", req.Host)
	}

	if creds.IdentityToken != "" {
		res.Secret = creds.IdentityToken
	} else {
		res.Username = creds.Username
		res.Secret = creds.Password
	}

	return res, nil
}

// decodeAuth decodes a base64 encoded string and returns username and password
func decodeAuth(authStr string) (string, string, error) {
	if authStr == "" {
		return "", "", nil
	}

	decLen := base64.StdEncoding.DecodedLen(len(authStr))
	decoded := make([]byte, decLen)
	authByte := []byte(authStr)
	n, err := base64.StdEncoding.Decode(decoded, authByte)
	if err != nil {
		return "", "", err
	}
	if n > decLen {
		return "", "", errors.Errorf("Something went wrong decoding auth config")
	}
	arr := strings.SplitN(string(decoded), ":", 2)
	if len(arr) != 2 {
		return "", "", errors.Errorf("Invalid auth configuration file")
	}
	password := strings.Trim(arr[1], "\x00")
	return arr[0], password, nil
}
