package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// defaultTimeout defines default timeout for HTTP requests.
const defaultTimeout = time.Second * 5

var (
	githubCI      = os.Getenv("GITHUB_ACTION") != ""
	gitlabCI      = os.Getenv("GITLAB_CI") != ""
	gitlabCIDebug = os.Getenv("GITLAB_CI_DEBUG") != ""
)

func main() {
	switch {
	case githubCI:
		info("🐣 Starting work with GitHub CI")
	case gitlabCI:
		info("🐣 Starting work with GitLab CI")
	default:
		stringError("🤡 Unknown CI server")
		os.Exit(1)
	}

	// Tenant domain name (e.g. example.secretsvaultcloud.com).
	domain := os.Getenv("DOMAIN")
	if domain == "" {
		stringError("DOMAIN variable must be specified")
		os.Exit(1)
	}
	// Client ID for authentication.
	clientId := os.Getenv("CLIENT_ID")
	if clientId == "" {
		stringError("CLIENT_ID variable must be specified")
		os.Exit(1)
	}
	// Client Secret for authentication.
	clientSecret := os.Getenv("CLIENT_SECRET")
	if clientSecret == "" {
		stringError("CLIENT_SECRET variable must be specified")
		os.Exit(1)
	}
	// Data to retrieve from DSV in format `<path> <data key> as <output key>`.
	retrieve := os.Getenv("RETRIEVE")
	if retrieve == "" {
		stringError("RETRIEVE variable must be specified")
		os.Exit(1)
	}
	// Set environment variables in GITHUB. Required GITHUB_ENV environment variable to be a valid path to a file.
	setEnv := false
	if (githubCI && os.Getenv("SET_ENV") != "") || gitlabCI {
		setEnv = true
	}
	retrieveData, err := parseRetrieveFlag(retrieve)
	if err != nil {
		printError(err)
		os.Exit(1)
	}
	if err := run(domain, clientId, clientSecret, setEnv, retrieveData); err != nil {
		printError(err)
		os.Exit(1)
	}
}

func run(domain, clientId, clientSecret string, setEnv bool, retrieveData map[string]map[string]string) error {
	apiEndpoint := fmt.Sprintf("https://%s/v1", domain)
	httpClient := &http.Client{Timeout: defaultTimeout}

	info("🔑 Fetching access token...")
	token, err := dsvGetToken(httpClient, apiEndpoint, clientId, clientSecret)
	if err != nil {
		debugf("authentication failed: %v", err)
		return fmt.Errorf("unable to get token")
	}
	debug("Got access token")

	envFile, err := openEnvFile(setEnv)
	if err != nil {
		return err
	}
	defer envFile.Close()

	info("✨ Fetching secret(s) from DSV...")
	debugf("RETRIEVE: %#v\n", retrieveData)
	for path, dataMap := range retrieveData {
		debugf("Fetching secret at path %q", path)

		secret, err := dsvGetSecret(httpClient, apiEndpoint, token, path)
		if err != nil {
			debugf("failed to fetch secret from DSV: %v", err)
			return fmt.Errorf("unable to get secret")
		}
		debugf("Got secret at path %q", path)

		secretData, ok := secret["data"].(map[string]interface{})
		if !ok {
			debugf("cannot get secret data from '%s' secret", path)
			return fmt.Errorf("cannot parse secret")
		}

		for secretDataKey, outputKey := range dataMap {
			debugf("Getting %s field from secret at path %s", secretDataKey, path)
			secretValue, ok := secretData[secretDataKey].(string)
			if !ok {
				debugf("cannot get '%s' from '%s' secret data", secretDataKey, path)
				return fmt.Errorf("cannot parse secret")
			}
			debugf("Got %s field from secret at path %s", secretDataKey, path)

			if githubCI {
				actionSetOutput(outputKey, secretValue)
				debugf("Output %s has been set as value '%s' from secret at path %s",
					strings.ToUpper(outputKey), secretDataKey, path)
			}
			if setEnv {
				if err := exportVariable(envFile, outputKey, secretValue); err != nil {
					debugf("exporting variable error: %v", err)
					return fmt.Errorf("cannot set environment variable")
				}
				debugf("Environment variable %s has been set as value %s from %s secret",
					strings.ToUpper(outputKey), secretDataKey, path)
			}
		}
	}
	return nil
}

func parseRetrieveFlag(retrieve string) (map[string]map[string]string, error) {
	pathRegexp := regexp.MustCompile(`^[a-zA-Z0-9:\/@\+._-]+$`)
	whitespaces := regexp.MustCompile(`\s+`)

	result := make(map[string]map[string]string)

	for _, row := range strings.Split(retrieve, "\n") {
		row = strings.TrimSpace(row)
		if row == "" {
			continue
		}
		row = whitespaces.ReplaceAllString(row, " ")

		tokens := strings.Split(row, " ")

		if len(tokens) != 4 {
			return nil, fmt.Errorf("failed to parse '%s'. "+
				"each 'retrieve' row must contain '<secret path> <secret data key> as <output key>' separated by spaces and/or tabs", row)
		}

		var (
			path      = tokens[0]
			dataKey   = tokens[1]
			outputKey = tokens[3]
		)
		if !pathRegexp.MatchString(path) {
			return nil, fmt.Errorf("failed to parse secret path '%s': "+
				"secret path may contain only letters, numbers, underscores, dashes, @, pluses and periods separated by colon or slash",
				path)
		}

		if _, ok := result[path]; !ok {
			result[path] = make(map[string]string)
		}
		result[path][dataKey] = outputKey
	}

	return result, nil
}

type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

func dsvGetToken(c httpClient, apiEndpoint, cid, csecret string) (string, error) {
	body := []byte(fmt.Sprintf(
		`{"grant_type":"client_credentials","client_id":"%s","client_secret":"%s"}`,
		cid, csecret,
	))
	endpoint := apiEndpoint + "/token"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("could not build request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Delinea-DSV-Client", "gh-action")

	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("API call failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("POST %s: %s", endpoint, resp.Status)
	}

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("could not read response body: %v", err)
	}
	tokenRespData := make(map[string]interface{})
	err = json.Unmarshal(body, &tokenRespData)
	if err != nil {
		return "", fmt.Errorf("could not unmarshal response body: %v", err)
	}

	token, strExists := tokenRespData["accessToken"].(string)
	if !strExists {
		return "", fmt.Errorf("could not read access token from response")
	}
	return token, nil
}

func dsvGetSecret(c httpClient, apiEndpoint, accessToken, secretPath string) (map[string]interface{}, error) {
	endpoint := apiEndpoint + "/secrets/" + secretPath
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("could not build request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Delinea-DSV-Client", "gh-action")
	req.Header.Set("Authorization", accessToken)

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API call failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", endpoint, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("could not read response body: %v", err)
	}
	secret := make(map[string]interface{})
	err = json.Unmarshal(body, &secret)
	if err != nil {
		return nil, fmt.Errorf("could not unmarshal response body: %v", err)
	}
	return secret, nil
}

func debug(s string) {
	if githubCI {
		fmt.Printf("::debug::%s\n", s)
	} else if gitlabCI && gitlabCIDebug {
		fmt.Printf("##[debug]\x1b[94m%s\x1b[0m\n", s)
	}
}

func debugf(format string, args ...interface{}) {
	debug(fmt.Sprintf(format, args...))
}

func info(s string) {
	if githubCI {
		fmt.Println(s)
	} else if gitlabCI {
		fmt.Printf("\x1b[92m%s\x1b[0m\n", s)
	}

}

func printError(err error) {
	if githubCI {
		fmt.Printf("::error::%v\n", err)
	} else if gitlabCI {
		fmt.Printf("\x1b[91m%v\x1b[0m\n", err)
	}
}

func stringError(s string) {
	if githubCI {
		fmt.Printf("::error::%s\n", s)
	} else if gitlabCI {
		fmt.Printf("\x1b[91m%s\x1b[0m\n", s)
	}
}

func actionSetOutput(key, val string) {
	fmt.Printf("::set-output name=%s::%s\n", key, val)
}

func openEnvFile(setEnv bool) (*os.File, error) {
	var (
		envFile *os.File
		err     error
	)
	if gitlabCI {
		jobName := os.Getenv("CI_JOB_NAME")
		if jobName == "" {
			return nil, fmt.Errorf("CI_JOB_NAME environment is not defined")
		}
		pwd := os.Getenv("CI_PROJECT_PATH")
		if pwd == "" {
			return nil, fmt.Errorf("CI_PROJECT_PATH environment is not defined")
		}
		envFileName := path.Join("/builds/", pwd, jobName)
		envFile, err = os.OpenFile(envFileName, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, fmt.Errorf("cannot open file %s: %v", envFileName, err)
		}
	} else if githubCI && setEnv {
		envFileName := os.Getenv("GITHUB_ENV")
		if envFileName == "" {
			return nil, fmt.Errorf("GITHUB_ENV environment is not defined")
		}
		envFile, err = os.OpenFile(envFileName, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, fmt.Errorf("cannot open file %s: %v", envFileName, err)
		}
	}
	return envFile, nil
}

func exportVariable(envFile *os.File, key, val string) error {
	if _, err := envFile.WriteString(fmt.Sprintf("%s=%s\n", strings.ToUpper(key), val)); err != nil {
		return fmt.Errorf("could not update %s environment file: %v", envFile.Name(), err)
	}
	return nil
}
