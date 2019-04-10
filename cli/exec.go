package cli

import (
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/stoggi/aws-oidc/provider"

	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

type ExecConfig struct {
	RoleArn      string
	Duration     int64
	ProviderURL  string
	ClientID     string
	ClientSecret string
	PKCE         bool
	Nonce        bool
	ReAuth       bool
	AgentCommant []string
}

// json metadata for AWS credential process. Ref: https://docs.aws.amazon.com/cli/latest/topic/config-vars.html#sourcing-credentials-from-external-processes
type AwsCredentialHelperData struct {
	Version         int    `json:"Version"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
	Expiration      string `json:"Expiration,omitempty"`
}

func ConfigureExec(app *kingpin.Application, config *GlobalConfig) {

	execConfig := ExecConfig{}

	cmd := app.Command("exec", "Execute a command with temporary AWS credentials")

	cmd.Default()

	cmd.Flag("role_arn", "The AWS role you want to assume").
		Required().
		StringVar(&execConfig.RoleArn)

	cmd.Flag("duration", "The duration to assume the role for in seconds").
		Default("3600").
		Int64Var(&execConfig.Duration)

	cmd.Flag("provider_url", "The OpenID Connect Provider URL").
		Required().
		StringVar(&execConfig.ProviderURL)

	cmd.Flag("client_id", "The OpenID Connect Client ID").
		Required().
		StringVar(&execConfig.ClientID)

	cmd.Flag("client_secret", "The OpenID Connect Client Secret").
		Default("").
		StringVar(&execConfig.ClientSecret)

	cmd.Flag("pkce", "Use PKCE in the OIDC code flow").
		Default("true").
		BoolVar(&execConfig.PKCE)

	cmd.Flag("nonce", "Require a nonce included and verified in the token").
		Default("true").
		BoolVar(&execConfig.Nonce)

	cmd.Flag("reauth", "Require reauthentication by the identity provider").
		Default("false").
		BoolVar(&execConfig.ReAuth)

	cmd.Arg("agent", "The executable and arguments of the local browser to use").
		Default("open", "{}").
		StringsVar(&execConfig.AgentCommant)

	cmd.Action(func(c *kingpin.ParseContext) error {
		ExecCommand(app, config, &execConfig)
		return nil
	})
}

func ExecCommand(app *kingpin.Application, config *GlobalConfig, execConfig *ExecConfig) {

	providerConfig := &provider.ProviderConfig{
		ClientID:     execConfig.ClientID,
		ClientSecret: execConfig.ClientSecret,
		ProviderURL:  execConfig.ProviderURL,
		PKCE:         execConfig.PKCE,
		Nonce:        execConfig.Nonce,
		ReAuth:       execConfig.ReAuth,
		AgentCommand: execConfig.AgentCommant,
	}

	authResult, err := provider.Authenticate(providerConfig)
	app.FatalIfError(err, "Error authenticating to identity provider: %v", err)

	svc := sts.New(session.New())
	input := &sts.AssumeRoleWithWebIdentityInput{
		DurationSeconds:  aws.Int64(execConfig.Duration),
		RoleArn:          aws.String(execConfig.RoleArn),
		RoleSessionName:  aws.String(authResult.Token.Subject),
		WebIdentityToken: aws.String(authResult.JWT),
	}

	assumeRoleResult, err := svc.AssumeRoleWithWebIdentity(input)
	app.FatalIfError(err, "Unable to assume role: %v", err)

	expiry := *assumeRoleResult.Credentials.Expiration
	credentialData := AwsCredentialHelperData{
		Version:         1,
		AccessKeyID:     *assumeRoleResult.Credentials.AccessKeyId,
		SecretAccessKey: *assumeRoleResult.Credentials.SecretAccessKey,
		SessionToken:    *assumeRoleResult.Credentials.SessionToken,
		Expiration:      expiry.Format("2006-01-02T15:04:05Z"),
	}

	json, err := json.Marshal(&credentialData)
	if err != nil {
		app.Fatalf("Error creating credential json")
	}
	fmt.Printf(string(json))
}
