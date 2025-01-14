package config

import (
	"context"
	"fmt"
	"github.com/akeylesslabs/akeyless-go/v4"
	"io"
	"log"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	encoding_ex "akeyless.io/akeyless-main-repo/go/src/infra/encoding-ex"
	"github.com/akeylesslabs/akeyless-go-cloud-id/cloudprovider/aws"
	"github.com/akeylesslabs/akeyless-go-cloud-id/cloudprovider/azure"
	"github.com/akeylesslabs/akeyless-go-cloud-id/cloudprovider/gcp"
)

const (
	authenticationInterval   = time.Second * 870 // 14.5 minutes - Relevant only for non-UID authentications
	uidTokenRotationInterval = time.Second * 120
	DefServiceAccountFile    = "/var/run/secrets/kubernetes.io/serviceaccount/token"
)

var (
	akeylessAuthToken string
	mutexAuthToken    = &sync.RWMutex{}
	authenticator     = func(ctx context.Context, aklClient *akeyless.V2ApiService) error { return nil }
)

func setAuthToken(t string) {
	mutexAuthToken.Lock()
	defer mutexAuthToken.Unlock()

	akeylessAuthToken = t
}

func GetAuthToken() string {
	mutexAuthToken.RLock()
	defer mutexAuthToken.RUnlock()

	return akeylessAuthToken
}

func (c *Config) authenticate(ctx context.Context, aklClient *akeyless.V2ApiService, authBody *akeyless.Auth) error {
	authBody.SetAccessId(c.AkeylessAccessID)

	authOut, _, err := aklClient.Auth(ctx).Body(*authBody).Execute()
	if err != nil {
		return fmt.Errorf("authentication failed %v, %w", c.AkeylessGatewayURL, err)
	}

	setAuthToken(authOut.GetToken())
	return nil
}

func (c *Config) authWithAccessKey(ctx context.Context, aklClient *akeyless.V2ApiService) error {
	authBody := akeyless.NewAuthWithDefaults()
	authBody.SetAccessType(string(AccessKey))
	authBody.SetAccessKey(c.AkeylessAccessKey)
	err := c.authenticate(ctx, aklClient, authBody)

	if err != nil {
		log.Printf("authWithAccessKey ERR: %v", err.Error())
	}
	return err
}

func (c *Config) authWithAWS(ctx context.Context, aklClient *akeyless.V2ApiService) error {
	authBody := akeyless.NewAuthWithDefaults()
	authBody.SetAccessType(string(AWSIAM))
	cloudId, err := aws.GetCloudId()
	if err != nil {
		return fmt.Errorf("requested access type %v but failed to get cloud ID, error: %v", AWSIAM, err)
	}
	authBody.SetCloudId(cloudId)
	err = c.authenticate(ctx, aklClient, authBody)

	if err != nil {
		log.Printf("authWithAWS ERR: %v", err.Error())
	}
	return err
}

func (c *Config) authWithAzure(ctx context.Context, aklClient *akeyless.V2ApiService) error {
	authBody := akeyless.NewAuthWithDefaults()
	authBody.SetAccessType(string(AzureAD))
	cloudId, err := azure.GetCloudId(c.AkeylessAzureObjectID)
	if err != nil {
		return fmt.Errorf("requested access type %v but failed to get cloud ID, error: %v", AzureAD, err)
	}
	authBody.SetCloudId(cloudId)
	err = c.authenticate(ctx, aklClient, authBody)

	if err != nil {
		log.Printf("authWithAzure ERR: %v", err.Error())
	}
	return err
}

func (c *Config) authWithGCP(ctx context.Context, aklClient *akeyless.V2ApiService) error {
	authBody := akeyless.NewAuthWithDefaults()
	authBody.SetAccessType(string(GCP))
	cloudId, err := gcp.GetCloudID(c.AkeylessGCPAudience)
	if err != nil {
		return fmt.Errorf("requested access type %v but failed to get cloud ID, error: %v", GCP, err)
	}
	authBody.SetCloudId(cloudId)
	err = c.authenticate(ctx, aklClient, authBody)

	if err != nil {
		log.Printf("authWithGCP ERR: %v", err.Error())
	}
	return err
}

func (c *Config) authWithK8S(ctx context.Context, aklClient *akeyless.V2ApiService) error {
	authBody := akeyless.NewAuthWithDefaults()
	authBody.SetAccessType(string(K8S))
	authBody.SetK8sAuthConfigName(c.AkeylessK8sAuthConfigName)
	jwtString, err := readK8SServiceAccountJWT()
	if err != nil {
		return fmt.Errorf("failed to read JWT with Kubernetes Auth from %v. error: %v", DefServiceAccountFile, err.Error())
	}
	authBody.SetK8sServiceAccountToken(jwtString)
	err = c.authenticate(ctx, aklClient, authBody)

	if err != nil {
		log.Printf("authWithK8s ERR: %v", err.Error())
	}
	return err
}

func (c *Config) rotateUIDToken(ctx context.Context, aklClient *akeyless.V2ApiService) error {
	// Get current token
	currToken := GetAuthToken()

	// rotate token
	log.Println("rotating UID token")
	body := akeyless.UidRotateToken{
		UidToken: akeyless.PtrString(currToken),
	}
	authOut, _, err := aklClient.UidRotateToken(ctx).Body(body).Execute()
	if err != nil {
		return fmt.Errorf("failed to rotate UID token %w", err)
	}
	newToken := authOut.GetToken()
	if newToken == "" {
		return fmt.Errorf("rotated uid token returned empty")
	}

	// Set new token
	setAuthToken(newToken)
	log.Println("successfully rotated UID token")
	return nil
}

// readK8SServiceAccountJWT reads the JWT data for the Agent to submit to Akeyless Gateway.
func readK8SServiceAccountJWT() (string, error) {
	data, err := os.Open(DefServiceAccountFile)
	if err != nil {
		return "", err
	}
	defer data.Close()

	contentBytes, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}

	a := strings.TrimSpace(string(contentBytes))

	return encoding_ex.Base64Encode([]byte(a)), nil
}

func (c *Config) StartAuthentication(ctx context.Context, closed chan bool) error {
	accType := c.AkeylessAccessType

	switch accessType(accType) {
	case AccessKey:
		authenticator = c.authWithAccessKey

	case AWSIAM:
		authenticator = c.authWithAWS

	case AzureAD:
		authenticator = c.authWithAzure

	case GCP:
		authenticator = c.authWithGCP

	case K8S:
		authenticator = c.authWithK8S
	}

	if accessType(accType) == UniversalIdentity {
		// Rotate UID token every uidTokenRotationInterval seconds
		runForeverWithContext(ctx, func() error {
			ticker := time.NewTicker(uidTokenRotationInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					closed <- true
					return nil
				case <-ticker.C:
					err := c.rotateUIDToken(ctx, AklClient)
					if err != nil {
						return err
					}
				}
			}
		}, closed)
	} else {
		// Get new token every authenticationInterval seconds
		runForeverWithContext(ctx, func() error {
			ticker := time.NewTicker(authenticationInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					closed <- true
					return nil
				case <-ticker.C:
					log.Println("retrieving new token")
					err := authenticator(ctx, AklClient)
					if err != nil {
						return err
					}
					log.Println("successfully retrieved new token")
				}
			}
		}, closed)
	}

	return nil
}

func getFunctionName(i interface{}) string {
	return runtime.FuncForPC(reflect.ValueOf(i).Pointer()).Name()
}

func runForeverWithContext(ctx context.Context, fn func() error, notifier chan bool) {
	runForeverWithContextEx(ctx, fn, "daemon", notifier)
}

func runForeverWithContextEx(ctx context.Context, fn func() error, routineType string, notifier chan bool) {
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				notifier <- true
				return
			case <-t.C:
				func() {
					err := fn()
					if err != nil {
						log.Printf("%s %s ended with an error. %s", routineType, getFunctionName(fn), err)
					}
				}()
			}
		}
	}()
}
