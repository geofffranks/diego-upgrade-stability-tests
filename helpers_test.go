package upgrade_test

import (
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/cf-test-helpers/cf"
	"github.com/cloudfoundry-incubator/cf-test-helpers/generator"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
	"github.com/tedsuo/ifrit"
)

const (
	CF_USER           = "admin"
	CF_PASSWORD       = "admin"
	APP_ROUTE_PATTERN = "http://%s.%s"
)

func boshCmd(manifest, action, completeMsg string) {
	args := []string{"-n"}
	if manifest != "" {
		args = append(args, "-d", manifest)
	}
	args = append(args, strings.Split(action, " ")...)
	cmd := bosh(args...)
	sess, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Eventually(sess, BOSH_DEPLOY_TIMEOUT).Should(gexec.Exit(0))
	Expect(sess).To(gbytes.Say(completeMsg))
}

func smokeTestDiego() {
	smokeTestApp := newCfApp("smoke-test", 0)
	// push new app
	smokeTestApp.Push()

	// destroy after test finishes
	defer smokeTestApp.Destroy()

	// verify scaling up
	smokeTestApp.Scale(2)

	// verify ssh functionality
	smokeTestApp.VerifySsh(0)
	smokeTestApp.VerifySsh(1)

	// verify scaling down
	smokeTestApp.Scale(1)
}

type cfApp struct {
	appName, orgName, spaceName string
	appRoute                    url.URL
	failedCurls                 int
	maxFailedCurls              int
}

func newCfApp(appNamePrefix string, maxFailedCurls int) *cfApp {
	appName := appNamePrefix + "-" + generator.RandomName()
	rawUrl := fmt.Sprintf(APP_ROUTE_PATTERN, appName, config.OverrideDomain)
	appUrl, err := url.Parse(rawUrl)
	if err != nil {
		panic(err)
	}
	return &cfApp{
		appName:        appName,
		appRoute:       *appUrl,
		orgName:        "org-" + generator.RandomName(),
		spaceName:      "space-" + generator.RandomName(),
		maxFailedCurls: maxFailedCurls,
	}
}

func (a *cfApp) Push() {
	// create org and space
	Eventually(func() int {
		return cf.Cf("login", "-a", "api."+config.OverrideDomain, "-u", CF_USER, "-p", CF_PASSWORD, "--skip-ssl-validation").Wait().ExitCode()
	}).Should(Equal(0))
	Eventually(cf.Cf("create-org", a.orgName)).Should(gexec.Exit(0))
	Eventually(cf.Cf("target", "-o", a.orgName)).Should(gexec.Exit(0))
	Eventually(cf.Cf("create-space", a.spaceName)).Should(gexec.Exit(0))
	Eventually(cf.Cf("target", "-s", a.spaceName)).Should(gexec.Exit(0))

	// push app
	Eventually(cf.Cf("push", a.appName, "-p", "dora", "-i", "1", "-b", "ruby_buildpack"), 5*time.Minute).Should(gexec.Exit(0))
	Eventually(cf.Cf("logs", a.appName, "--recent")).Should(gbytes.Say("[HEALTH/0]"))
	curlAppMain := func() string {
		response, err := a.Curl("")
		if err != nil {
			return ""
		}
		return response
	}

	Eventually(curlAppMain).Should(ContainSubstring("Hi, I'm Dora!"))
}

func (a *cfApp) Scale(numInstances int) {
	Eventually(cf.Cf("target", "-o", a.orgName, "-s", a.spaceName)).Should(gexec.Exit(0))
	Eventually(cf.Cf("scale", a.appName, "-i", strconv.Itoa(numInstances))).Should(gexec.Exit(0))
	Eventually(func() int {
		found := make(map[string]struct{})
		for i := 0; i < numInstances*2; i++ {
			id, err := a.Curl("id")
			if err != nil {
				log.Printf("Failed Curling While Scaling: %s\n", err.Error())
				return -1
			}
			found[id] = struct{}{}
			time.Sleep(1 * time.Second)
		}
		return len(found)
	}).Should(Equal(numInstances))
}

func (a *cfApp) VerifySsh(instanceIndex int) {
	envCmd := cf.Cf("ssh", a.appName, "-i", strconv.Itoa(instanceIndex), "-c", `"/usr/bin/env"`)
	Expect(envCmd.Wait()).To(gexec.Exit(0))

	output := string(envCmd.Buffer().Contents())

	Expect(string(output)).To(MatchRegexp(fmt.Sprintf(`VCAP_APPLICATION=.*"application_name":"%s"`, a.appName)))
	Expect(string(output)).To(MatchRegexp(fmt.Sprintf("INSTANCE_INDEX=%d", instanceIndex)))

	Eventually(cf.Cf("logs", a.appName, "--recent")).Should(gbytes.Say("Successful remote access"))
	Eventually(cf.Cf("events", a.appName)).Should(gbytes.Say("audit.app.ssh-authorized"))
}

func (a *cfApp) Destroy() {
	Eventually(cf.Cf("delete-org", "-f", a.orgName)).Should(gexec.Exit(0))
}

func (testApp *cfApp) NewPoller() ifrit.RunFunc {
	return func(signals <-chan os.Signal, ready chan<- struct{}) error {
		defer GinkgoRecover()

		close(ready)

		curlTimer := time.NewTimer(0)
		for {
			select {
			case <-curlTimer.C:
				_, err := testApp.Curl("id")
				Expect(err).NotTo(HaveOccurred(), "continuous polling failed")

				curlTimer.Reset(2 * time.Second)

			case <-signals:
				By(fmt.Sprintf("exiting continuous test poller (%d failed curl requests)\n", testApp.failedCurls))
				testApp.failedCurls = 0
				return nil
			}
		}
	}
}

func (a *cfApp) Curl(endpoint string) (string, error) {
	endpointUrl := a.appRoute
	endpointUrl.Path = endpoint

	url := endpointUrl.String()

	statusCode, body, err := curl(url)
	if err != nil {
		return "", err
	}

	switch {
	case statusCode == 200:
		return string(body), nil

	case a.shouldRetryRequest(statusCode):
		fmt.Fprintln(GinkgoWriter, "RETRYING CURL", newCurlErr(url, statusCode, body).Error())
		a.failedCurls++
		time.Sleep(2 * time.Second)
		return a.Curl(endpoint)

	default:
		err := newCurlErr(url, statusCode, body)
		fmt.Fprintln(GinkgoWriter, "FAILED CURL", err.Error())
		return "", err
	}
}

func (a *cfApp) shouldRetryRequest(statusCode int) bool {
	if statusCode == 503 || statusCode == 404 {
		return a.failedCurls < a.maxFailedCurls
	}
	return false
}

func curl(url string) (statusCode int, body string, err error) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, "", err
	}

	defer resp.Body.Close()

	bytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0, "", err
	}
	return resp.StatusCode, string(bytes), nil
}

func newCurlErr(url string, statusCode int, body string) error {
	return fmt.Errorf("Endpoint: %s, Status Code: %d, Body: %s", url, statusCode, body)
}
