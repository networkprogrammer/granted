package cfaws

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sso"
	ssotypes "github.com/aws/aws-sdk-go-v2/service/sso/types"
	"github.com/aws/aws-sdk-go-v2/service/ssooidc"
	ssooidctypes "github.com/aws/aws-sdk-go-v2/service/ssooidc/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/bigkevmcd/go-configparser"
	grantedConfig "github.com/common-fate/granted/pkg/config"
	"github.com/common-fate/granted/pkg/debug"
	"github.com/fatih/color"
	"github.com/pkg/browser"
	"github.com/pkg/errors"
)

// Implements Assumer
type AwsSsoAssumer struct {
}

func (asa *AwsSsoAssumer) AssumeTerminal(ctx context.Context, c *Profile, configOpts ConfigOpts) (aws.Credentials, error) {
	return c.SSOLogin(ctx, configOpts)
}

func (asa *AwsSsoAssumer) AssumeConsole(ctx context.Context, c *Profile, configOpts ConfigOpts) (aws.Credentials, error) {
	return c.SSOLogin(ctx, configOpts)
}

func (asa *AwsSsoAssumer) Type() string {
	return "AWS_SSO"
}

// Matches the profile type on whether it is an sso profile by checking for ssoaccountid.
func (asa *AwsSsoAssumer) ProfileMatchesType(rawProfile configparser.Dict, parsedProfile config.SharedConfig) bool {
	return parsedProfile.SSOAccountID != ""
}

func (c *Profile) SSOLogin(ctx context.Context, configOpts ConfigOpts) (aws.Credentials, error) {

	rootProfile := c
	requiresAssuming := false
	if len(c.Parents) > 0 {
		rootProfile = c.Parents[0]

		requiresAssuming = true
	}

	ssoTokenKey := rootProfile.AWSConfig.SSOStartURL
	cfg := aws.NewConfig()
	cfg.Region = rootProfile.AWSConfig.SSORegion

	cachedToken := GetValidCachedToken(ssoTokenKey)
	var accessToken *string
	if cachedToken == nil {
		newSSOToken, err := SSODeviceCodeFlowFromStartUrl(ctx, *cfg, rootProfile.AWSConfig.SSOStartURL)
		if err != nil {
			return aws.Credentials{}, err
		}

		StoreSSOToken(ssoTokenKey, *newSSOToken)
		accessToken = &newSSOToken.AccessToken
	} else {
		accessToken = &cachedToken.AccessToken
	}

	// create sso client
	ssoClient := sso.NewFromConfig(*cfg)
	res, err := ssoClient.GetRoleCredentials(ctx, &sso.GetRoleCredentialsInput{AccessToken: accessToken, AccountId: &rootProfile.AWSConfig.SSOAccountID, RoleName: &rootProfile.AWSConfig.SSORoleName})
	if err != nil {
		serr, ok := err.(*smithy.OperationError)
		if ok {
			// If the err is of type ForbiddenRequest then user may be able
			// to request access to the role if they are using Granted Approvals.
			// Display an error message with the request URL, or a prompt
			// to set up the request URL if it's empty.
			if httpErr, ok := serr.Err.(*awshttp.ResponseError); ok {
				if httpErr.HTTPStatusCode() == http.StatusForbidden {
					if hasGrantedPrefix(c.RawConfig) {
						gConf, loadErr := grantedConfig.Load()
						if loadErr != nil {
							return aws.Credentials{}, errors.Wrapf(err, "loading Granted config: %s", loadErr.Error())
						}

						requestURLMsg, err := GetGrantedApprovalsURL(c.RawConfig, *gConf, c.AWSConfig.SSORoleName, c.AWSConfig.SSOAccountID)
						if err != nil {
							return aws.Credentials{}, err
						}
						return aws.Credentials{}, errors.New(serr.Error() + "\n" + requestURLMsg)
					}
				}
			}

		}

		var unauthorised *ssotypes.UnauthorizedException
		if errors.As(err, &unauthorised) {
			// possible error with the access token we used, in this case we should clear our cached token and request a new one if the user tries again
			ClearSSOToken(ssoTokenKey)
		}
		return aws.Credentials{}, err
	}
	rootCreds := TypeRoleCredsToAwsCreds(*res.RoleCredentials)
	credProvider := &CredProv{rootCreds}

	if requiresAssuming {
		// return creds, nil
		toAssume := append([]*Profile{}, c.Parents[1:]...)
		toAssume = append(toAssume, c)
		for i, p := range toAssume {
			region, err := c.Region(ctx)
			if err != nil {
				return aws.Credentials{}, err
			}
			// in order to support profiles which do not specify a region, we use the default region when assuming the role
			stsClient := sts.New(sts.Options{Credentials: aws.NewCredentialsCache(credProvider), Region: region})
			stsp := stscreds.NewAssumeRoleProvider(stsClient, p.AWSConfig.RoleARN, func(aro *stscreds.AssumeRoleOptions) {
				// all configuration goes in here for this profile
				if p.AWSConfig.RoleSessionName != "" {
					aro.RoleSessionName = p.AWSConfig.RoleSessionName
				} else {
					aro.RoleSessionName = sessionName()
				}
				if p.AWSConfig.MFASerial != "" {
					aro.SerialNumber = &p.AWSConfig.MFASerial
					aro.TokenProvider = MfaTokenProvider
				} else if c.AWSConfig.MFASerial != "" {
					aro.SerialNumber = &c.AWSConfig.MFASerial
					aro.TokenProvider = MfaTokenProvider
				}
				aro.Duration = configOpts.Duration
				if p.AWSConfig.ExternalID != "" {
					aro.ExternalID = &p.AWSConfig.ExternalID
				}
			})
			stsCreds, err := stsp.Retrieve(ctx)
			if err != nil {
				return aws.Credentials{}, err
			}
			// only print for sub assumes because the final credentials are printed at the end of the assume command
			// this is here for visibility in to role traversals when assuming a final profile with sso
			if i < len(toAssume)-1 {
				green := color.New(color.FgGreen)
				green.Fprintf(color.Error, "\nAssumed parent profile: [%s](%s) session credentials will expire %s\n", p.Name, region, stsCreds.Expires.Local().String())
			}
			credProvider = &CredProv{stsCreds}

		}
	}
	return credProvider.Credentials, nil

}

// SSODeviceCodeFlowFromStartUrl contains all the steps to complete a device code flow to retrieve an SSO token
func SSODeviceCodeFlowFromStartUrl(ctx context.Context, cfg aws.Config, startUrl string) (*SSOToken, error) {
	ssooidcClient := ssooidc.NewFromConfig(cfg)

	register, err := ssooidcClient.RegisterClient(ctx, &ssooidc.RegisterClientInput{
		ClientName: aws.String("granted-cli-client"),
		ClientType: aws.String("public"),
		Scopes:     []string{"sso-portal:*"},
	})
	if err != nil {
		return nil, err
	}

	// authorize your device using the client registration response
	deviceAuth, err := ssooidcClient.StartDeviceAuthorization(ctx, &ssooidc.StartDeviceAuthorizationInput{

		ClientId:     register.ClientId,
		ClientSecret: register.ClientSecret,
		StartUrl:     aws.String(startUrl),
	})
	if err != nil {
		return nil, err
	}
	// trigger OIDC login. open browser to login. close tab once login is done. press enter to continue
	url := aws.ToString(deviceAuth.VerificationUriComplete)
	fmt.Fprintf(color.Error, "If browser is not opened automatically, please open link:\n%v\n", url)

	//check if sso browser path is set
	config, err := grantedConfig.Load()
	if err != nil {
		return nil, err
	}

	if config.CustomSSOBrowserPath != "" {
		cmd := exec.Command(config.CustomSSOBrowserPath, url)
		err = cmd.Start()
		if err != nil {
			// fail silently
			debug.Fprintf(debug.VerbosityDebug, color.Error, err.Error())
		} else {
			// detatch from this new process because it continues to run
			err = cmd.Process.Release()
			if err != nil {
				// fail silently
				debug.Fprintf(debug.VerbosityDebug, color.Error, err.Error())
			}
		}
	} else {
		err = browser.OpenURL(url)
		if err != nil {
			// fail silently
			debug.Fprintf(debug.VerbosityDebug, color.Error, err.Error())
		}
	}

	fmt.Fprintln(color.Error, "\nAwaiting authentication in the browser...")
	token, err := PollToken(ctx, ssooidcClient, *register.ClientSecret, *register.ClientId, *deviceAuth.DeviceCode, PollingConfig{CheckInterval: time.Second * 2, TimeoutAfter: time.Minute * 2})
	if err != nil {
		return nil, err
	}

	return &SSOToken{AccessToken: *token.AccessToken, Expiry: time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)}, nil
}

var ErrTimeout error = errors.New("polling for device authorization token timed out")

type PollingConfig struct {
	CheckInterval time.Duration
	TimeoutAfter  time.Duration
}

// PollToken will poll for a token and return it once the authentication/authorization flow has been completed in the browser
func PollToken(ctx context.Context, c *ssooidc.Client, clientSecret string, clientID string, deviceCode string, cfg PollingConfig) (*ssooidc.CreateTokenOutput, error) {
	start := time.Now()
	for {
		time.Sleep(cfg.CheckInterval)

		token, err := c.CreateToken(ctx, &ssooidc.CreateTokenInput{

			ClientId:     &clientID,
			ClientSecret: &clientSecret,
			DeviceCode:   &deviceCode,
			GrantType:    aws.String("urn:ietf:params:oauth:grant-type:device_code"),
		})
		var pendingAuth *ssooidctypes.AuthorizationPendingException
		if err == nil {
			return token, nil
		} else if !errors.As(err, &pendingAuth) {
			return nil, err
		}

		if time.Now().After(start.Add(cfg.TimeoutAfter)) {
			return nil, ErrTimeout
		}
	}
}
