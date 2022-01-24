package ddevapp

import (
	"bytes"
	"fmt"
	"github.com/Masterminds/sprig/v3"
	"github.com/drud/ddev/pkg/dockerutil"
	"github.com/drud/ddev/pkg/globalconfig"
	"github.com/drud/ddev/pkg/nodeps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

	"regexp"

	"runtime"

	"github.com/drud/ddev/pkg/fileutil"
	"github.com/drud/ddev/pkg/output"
	"github.com/drud/ddev/pkg/util"
	"github.com/drud/ddev/pkg/version"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

// Regexp pattern to determine if a hostname is valid per RFC 1123.
var hostRegex = regexp.MustCompile(`^(([a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9\-]*[a-zA-Z0-9])\.)*([A-Za-z0-9]|[A-Za-z0-9][A-Za-z0-9\-]*[A-Za-z0-9])$`)

// init() is for testing situations only, allowing us to override the default webserver type
// or caching behavior

func init() {
	// This is for automated testing only. It allows us to override the webserver type.
	if testWebServerType := os.Getenv("DDEV_TEST_WEBSERVER_TYPE"); testWebServerType != "" {
		nodeps.WebserverDefault = testWebServerType
	}
	if testNFSMount := os.Getenv("DDEV_TEST_USE_NFSMOUNT"); testNFSMount != "" {
		nodeps.NFSMountEnabledDefault = true
	}
	if testMutagen := os.Getenv("DDEV_TEST_USE_MUTAGEN"); testMutagen == "true" {
		nodeps.MutagenEnabledDefault = true
	}
	if os.Getenv("DDEV_TEST_NO_BIND_MOUNTS") == "true" {
		nodeps.NoBindMountsDefault = true
	}

}

// NewApp creates a new DdevApp struct with defaults set and overridden by any existing config.yml.
func NewApp(appRoot string, includeOverrides bool) (*DdevApp, error) {
	runTime := util.TimeTrack(time.Now(), fmt.Sprintf("ddevapp.NewApp(%s)", appRoot))
	defer runTime()

	app := &DdevApp{}

	if appRoot == "" {
		app.AppRoot, _ = os.Getwd()
	} else {
		app.AppRoot = appRoot
	}

	homeDir, _ := os.UserHomeDir()
	if appRoot == filepath.Dir(globalconfig.GetGlobalDdevDir()) || app.AppRoot == homeDir {
		return nil, fmt.Errorf("ddev config is not useful in your home directory (%s)", homeDir)
	}

	if !fileutil.FileExists(app.AppRoot) {
		return app, fmt.Errorf("project root %s does not exist", app.AppRoot)
	}
	app.ConfigPath = app.GetConfigPath("config.yaml")
	app.Type = nodeps.AppTypePHP
	app.PHPVersion = nodeps.PHPDefault
	app.ComposerVersion = nodeps.ComposerDefault
	app.WebserverType = nodeps.WebserverDefault
	app.NFSMountEnabled = nodeps.NFSMountEnabledDefault
	app.NFSMountEnabledGlobal = globalconfig.DdevGlobalConfig.NFSMountEnabledGlobal
	app.MutagenEnabled = nodeps.MutagenEnabledDefault
	app.MutagenEnabledGlobal = globalconfig.DdevGlobalConfig.MutagenEnabledGlobal
	app.FailOnHookFail = nodeps.FailOnHookFailDefault
	app.FailOnHookFailGlobal = globalconfig.DdevGlobalConfig.FailOnHookFailGlobal
	app.RouterHTTPPort = nodeps.DdevDefaultRouterHTTPPort
	app.RouterHTTPSPort = nodeps.DdevDefaultRouterHTTPSPort
	app.PHPMyAdminPort = nodeps.DdevDefaultPHPMyAdminPort
	app.PHPMyAdminHTTPSPort = nodeps.DdevDefaultPHPMyAdminHTTPSPort
	app.MailhogPort = nodeps.DdevDefaultMailhogPort
	app.MailhogHTTPSPort = nodeps.DdevDefaultMailhogHTTPSPort

	// Provide a default app name based on directory name
	app.Name = filepath.Base(app.AppRoot)

	// Gather containers to omit, adding ddev-router for gitpod
	app.OmitContainersGlobal = globalconfig.DdevGlobalConfig.OmitContainersGlobal
	if nodeps.IsGitpod() {
		app.OmitContainersGlobal = append(app.OmitContainersGlobal, "ddev-router")
	}
	app.ProjectTLD = nodeps.DdevDefaultTLD
	app.UseDNSWhenPossible = true

	app.WebImage = version.GetWebImage()

	// Load from file if available. This will return an error if the file doesn't exist,
	// and it is up to the caller to determine if that's an issue.
	if _, err := os.Stat(app.ConfigPath); !os.IsNotExist(err) {
		_, err = app.ReadConfig(includeOverrides)
		if err != nil {
			return app, fmt.Errorf("%v exists but cannot be read. It may be invalid due to a syntax error.: %v", app.ConfigPath, err)
		}
	}

	// Upgrade any pre-v1.19.0 config that has mariadb_version or mysql_version
	if app.MariaDBVersion != "" {
		app.Database = DatabaseDesc{Type: nodeps.MariaDB, Version: app.MariaDBVersion}
		app.MariaDBVersion = ""
	}
	if app.MySQLVersion != "" {
		app.Database = DatabaseDesc{Type: nodeps.MySQL, Version: app.MySQLVersion}
		app.MySQLVersion = ""
	}
	if app.Database.Type == "" {
		app.Database = DatabaseDefault
	}
	app.SetApptypeSettingsPaths()

	// Rendered yaml is not there until after ddev config or ddev start
	if fileutil.FileExists(app.ConfigPath) && fileutil.FileExists(app.DockerComposeFullRenderedYAMLPath()) {
		content, err := fileutil.ReadFileIntoString(app.DockerComposeFullRenderedYAMLPath())
		if err != nil {
			return app, err
		}
		err = app.UpdateComposeYaml(content)
		if err != nil {
			return app, err
		}
	}
	return app, nil
}

// GetConfigPath returns the path to an application config file specified by filename.
func (app *DdevApp) GetConfigPath(filename string) string {
	return filepath.Join(app.AppRoot, ".ddev", filename)
}

// WriteConfig writes the app configuration into the .ddev folder.
func (app *DdevApp) WriteConfig() error {

	// Work against a copy of the DdevApp, since we don't want to actually change it.
	appcopy := *app

	// Only set the images on write if non-default values have been specified.
	if appcopy.WebImage == version.GetWebImage() {
		appcopy.WebImage = ""
	}
	if appcopy.MailhogPort == nodeps.DdevDefaultMailhogPort {
		appcopy.MailhogPort = ""
	}
	if appcopy.MailhogHTTPSPort == nodeps.DdevDefaultMailhogHTTPSPort {
		appcopy.MailhogHTTPSPort = ""
	}
	if appcopy.PHPMyAdminPort == nodeps.DdevDefaultPHPMyAdminPort {
		appcopy.PHPMyAdminPort = ""
	}
	if appcopy.PHPMyAdminHTTPSPort == nodeps.DdevDefaultPHPMyAdminHTTPSPort {
		appcopy.PHPMyAdminHTTPSPort = ""
	}
	if appcopy.ProjectTLD == nodeps.DdevDefaultTLD {
		appcopy.ProjectTLD = ""
	}

	// We now want to reserve the port we're writing for HostDBPort and HostWebserverPort and so they don't
	// accidentally get used for other projects.
	err := app.UpdateGlobalProjectList()
	if err != nil {
		return err
	}

	// Don't write default working dir values to config
	defaults := appcopy.DefaultWorkingDirMap()
	for service, defaultWorkingDir := range defaults {
		if app.WorkingDir[service] == defaultWorkingDir {
			delete(appcopy.WorkingDir, service)
		}
	}

	err = PrepDdevDirectory(filepath.Dir(appcopy.ConfigPath))
	if err != nil {
		return err
	}

	cfgbytes, err := yaml.Marshal(appcopy)
	if err != nil {
		return err
	}

	// Append hook information and sample hook suggestions.
	cfgbytes = append(cfgbytes, []byte(ConfigInstructions)...)
	cfgbytes = append(cfgbytes, appcopy.GetHookDefaultComments()...)

	err = os.WriteFile(appcopy.ConfigPath, cfgbytes, 0644)
	if err != nil {
		return err
	}

	// Allow project-specific post-config action
	err = appcopy.PostConfigAction()
	if err != nil {
		return err
	}

	// Write example Dockerfiles into build directories
	contents := []byte(`
# You can copy this Dockerfile.example to Dockerfile to add configuration
# or packages or anything else to your webimage
ARG BASE_IMAGE
FROM $BASE_IMAGE
RUN npm install --global gulp-cli
`)

	err = WriteImageDockerfile(app.GetConfigPath("web-build")+"/Dockerfile.example", contents)
	if err != nil {
		return err
	}
	contents = []byte(`
# You can copy this Dockerfile.example to Dockerfile to add configuration
# or packages or anything else to your dbimage
ARG BASE_IMAGE
FROM $BASE_IMAGE
RUN echo "Built from ` + app.GetDBImage() + `" >/var/tmp/built-from.txt
`)

	err = WriteImageDockerfile(app.GetConfigPath("db-build")+"/Dockerfile.example", contents)
	if err != nil {
		return err
	}

	return nil
}

// UpdateGlobalProjectList updates any information about project that
// is tracked in global project list:
// - approot
// - configured host ports
// checks that configured host ports are not already
// reserved by another project
func (app *DdevApp) UpdateGlobalProjectList() error {
	portsToReserve := []string{}
	if app.HostDBPort != "" {
		portsToReserve = append(portsToReserve, app.HostDBPort)
	}
	if app.HostWebserverPort != "" {
		portsToReserve = append(portsToReserve, app.HostWebserverPort)
	}
	if app.HostHTTPSPort != "" {
		portsToReserve = append(portsToReserve, app.HostHTTPSPort)
	}

	if len(portsToReserve) > 0 {
		err := globalconfig.CheckHostPortsAvailable(app.Name, portsToReserve)
		if err != nil {
			return err
		}
	}
	err := globalconfig.ReservePorts(app.Name, portsToReserve)
	if err != nil {
		return err
	}
	err = globalconfig.SetProjectAppRoot(app.Name, app.AppRoot)
	if err != nil {
		return err
	}

	return nil
}

// ReadConfig reads project configuration from the config.yaml file
// It does not attempt to set default values; that's NewApp's job.
func (app *DdevApp) ReadConfig(includeOverrides bool) ([]string, error) {

	// Load config.yaml
	err := app.LoadConfigYamlFile(app.ConfigPath)
	if err != nil {
		return []string{}, fmt.Errorf("unable to load config file %s: %v", app.ConfigPath, err)
	}

	configOverrides := []string{}
	// Load config.*.y*ml after in glob order
	if includeOverrides {
		glob := filepath.Join(filepath.Dir(app.ConfigPath), "config.*.y*ml")
		configOverrides, err = filepath.Glob(glob)
		if err != nil {
			return []string{}, err
		}

		for _, item := range configOverrides {
			err = app.LoadConfigYamlFile(item)
			if err != nil {
				return []string{}, fmt.Errorf("unable to load config file %s: %v", item, err)
			}
		}
	}

	return append([]string{app.ConfigPath}, configOverrides...), nil
}

// LoadConfigYamlFile loads one config.yaml into app, overriding what might be there.
func (app *DdevApp) LoadConfigYamlFile(filePath string) error {
	source, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("could not find an active ddev configuration at %s have you run 'ddev config'? %v", app.ConfigPath, err)
	}

	// validate extend command keys
	err = validateHookYAML(source)
	if err != nil {
		return fmt.Errorf("invalid configuration in %s: %v", app.ConfigPath, err)
	}

	// ReadConfig config values from file.
	err = yaml.Unmarshal(source, app)
	if err != nil {
		return err
	}
	return nil
}

// WarnIfConfigReplace just messages user about whether config is being replaced or created
func (app *DdevApp) WarnIfConfigReplace() {
	if app.ConfigExists() {
		util.Warning("You are reconfiguring the project at %s.\nThe existing configuration will be updated and replaced.", app.AppRoot)
	} else {
		util.Success("Creating a new ddev project config in the current directory (%s)", app.AppRoot)
		util.Success("Once completed, your configuration will be written to %s\n", app.ConfigPath)
	}
}

// PromptForConfig goes through a set of prompts to receive user input and generate an Config struct.
func (app *DdevApp) PromptForConfig() error {

	app.WarnIfConfigReplace()

	for {
		err := app.promptForName()

		if err == nil {
			break
		}

		output.UserOut.Printf("%v", err)
	}

	if err := app.docrootPrompt(); err != nil {
		return err
	}

	err := app.AppTypePrompt()
	if err != nil {
		return err
	}

	err = app.ConfigFileOverrideAction()
	if err != nil {
		return err
	}

	err = app.ValidateConfig()
	if err != nil {
		return err
	}

	return nil
}

// ValidateProjectName checks to see if the project name works for a proper hostname
func ValidateProjectName(name string) error {
	match := hostRegex.MatchString(name)
	if !match {
		return fmt.Errorf("%s is not a valid project name. Please enter a project name in your configuration that will allow for a valid hostname. See https://en.wikipedia.org/wiki/Hostname#Restrictions_on_valid_hostnames for valid hostname requirements", name)
	}
	return nil
}

// ValidateConfig ensures the configuration meets ddev's requirements.
func (app *DdevApp) ValidateConfig() error {

	// validate project name
	if err := ValidateProjectName(app.Name); err != nil {
		return err
	}

	// validate hostnames
	for _, hn := range app.GetHostnames() {
		// If they have provided "*.<hostname>" then ignore the *. part.
		hn = strings.TrimPrefix(hn, "*.")
		if hn == "ddev.site" {
			return fmt.Errorf("wildcarding the full hostname or using 'ddev.site' as fqdn is not allowed because other projects would not work in that case")
		}
		if !hostRegex.MatchString(hn) {
			return fmt.Errorf("invalid hostname: %s. See https://en.wikipedia.org/wiki/Hostname#Restrictions_on_valid_hostnames for valid hostname requirements", hn).(invalidHostname)
		}
	}

	// validate apptype
	if !IsValidAppType(app.Type) {
		return fmt.Errorf("invalid app type: %s", app.Type).(invalidAppType)
	}

	// validate PHP version
	if !nodeps.IsValidPHPVersion(app.PHPVersion) {
		return fmt.Errorf("unsupported PHP version: %s, ddev (%s) only supports the following versions: %v", app.PHPVersion, runtime.GOARCH, nodeps.GetValidPHPVersions()).(invalidPHPVersion)
	}

	// validate webserver type
	if !nodeps.IsValidWebserverType(app.WebserverType) {
		return fmt.Errorf("unsupported webserver type: %s, ddev (%s) only supports the following webserver types: %s", app.WebserverType, runtime.GOARCH, nodeps.GetValidWebserverTypes()).(invalidWebserverType)
	}

	if !nodeps.IsValidOmitContainers(app.OmitContainers) {
		return fmt.Errorf("unsupported omit_containers: %s, ddev (%s) only supports the following for omit_containers: %s", app.OmitContainers, runtime.GOARCH, nodeps.GetValidOmitContainers()).(InvalidOmitContainers)
	}

	if !nodeps.IsValidDatabaseVersion(app.Database.Type, app.Database.Version) {
		return fmt.Errorf("unsupported database type/version: %s:%s, ddev %s only supports the following database types and versions: mariadb: %v, mysql: %v", app.Database.Type, app.Database.Version, runtime.GOARCH, nodeps.GetValidMariaDBVersions(), nodeps.GetValidMySQLVersions())
	}

	// golang on windows is not able to time.LoadLocation unless
	// go is installed... so skip validation on Windows
	if runtime.GOOS != "windows" {
		_, err := time.LoadLocation(app.Timezone)
		if err != nil {
			// golang on Windows is often not able to time.LoadLocation.
			// It often works if go is installed and $GOROOT is set, but
			// that's not the norm for our users.
			return fmt.Errorf("invalid timezone %s: %v", app.Timezone, err)
		}
	}

	return nil
}

// DockerComposeYAMLPath returns the absolute path to where the
// base generated yaml file should exist for this project.
func (app *DdevApp) DockerComposeYAMLPath() string {
	return app.GetConfigPath(".ddev-docker-compose-base.yaml")
}

// DockerComposeFullRenderedYAMLPath returns the absolute path to where the
// the complete generated yaml file should exist for this project.
func (app *DdevApp) DockerComposeFullRenderedYAMLPath() string {
	return app.GetConfigPath(".ddev-docker-compose-full.yaml")
}

// GetHostname returns the primary hostname of the app.
func (app *DdevApp) GetHostname() string {
	return strings.ToLower(app.Name) + "." + app.ProjectTLD
}

// GetHostnames returns an array of all the configured hostnames.
func (app *DdevApp) GetHostnames() []string {

	// Use a map to make sure that we have unique hostnames
	// The value is useless, so just use the int 1 for assignment.
	nameListMap := make(map[string]int)
	nameListArray := []string{}

	if !IsRouterDisabled(app) {
		for _, name := range app.AdditionalHostnames {
			name = strings.ToLower(name)
			nameListMap[name+"."+app.ProjectTLD] = 1
		}

		for _, name := range app.AdditionalFQDNs {
			name = strings.ToLower(name)
			nameListMap[name] = 1
		}

		// Make sure the primary hostname didn't accidentally get added, it will be prepended
		delete(nameListMap, app.GetHostname())

		// Now walk the map and extract the keys into an array.
		for k := range nameListMap {
			nameListArray = append(nameListArray, k)
		}
		sort.Strings(nameListArray)
		// We want the primary hostname to be first in the list.
		nameListArray = append([]string{app.GetHostname()}, nameListArray...)
	}
	return nameListArray
}

// WriteDockerComposeYAML writes a .ddev-docker-compose-base.yaml and related to the .ddev directory.
func (app *DdevApp) WriteDockerComposeYAML() error {
	var err error

	f, err := os.Create(app.DockerComposeYAMLPath())
	if err != nil {
		return err
	}
	defer util.CheckClose(f)

	rendered, err := app.RenderComposeYAML()
	if err != nil {
		return err
	}
	_, err = f.WriteString(rendered)
	if err != nil {
		return err
	}

	files, err := app.ComposeFiles()
	if err != nil {
		return err
	}
	fullContents, _, err := dockerutil.ComposeCmd(files, "config")
	if err != nil {
		return err
	}
	// Replace `docker-compose config`'s full-path usage with relative pathing
	// for https://youtrack.jetbrains.com/issue/WI-61976 - PhpStorm
	// This is an ugly an shortsighted approach, but otherwise we'd have to parse the yaml.
	// Note that this issue with docker-compose config was fixed in docker-compose 2.0.0RC4
	// so it's in Docker Desktop 4.1.0.
	// https://github.com/docker/compose/issues/8503#issuecomment-930969241
	fullContents = strings.Replace(fullContents, fmt.Sprintf("source: %s\n", app.AppRoot), "source: ../\n", -1)
	fullHandle, err := os.Create(app.DockerComposeFullRenderedYAMLPath())
	if err != nil {
		return err
	}
	_, err = fullHandle.WriteString(fullContents)
	if err != nil {
		return err
	}

	err = app.UpdateComposeYaml(fullContents)
	if err != nil {
		return err
	}
	return nil
}

// CheckCustomConfig warns the user if any custom configuration files are in use.
func (app *DdevApp) CheckCustomConfig() {

	// Get the path to .ddev for the current app.
	ddevDir := filepath.Dir(app.ConfigPath)

	customConfig := false
	if _, err := os.Stat(filepath.Join(ddevDir, "nginx-site.conf")); err == nil && app.WebserverType == nodeps.WebserverNginxFPM {
		util.Warning("Using custom nginx configuration in nginx-site.conf")
		customConfig = true
	}
	nginxFullConfigPath := app.GetConfigPath("nginx_full/nginx-site.conf")
	sigFound, _ := fileutil.FgrepStringInFile(nginxFullConfigPath, DdevFileSignature)
	if !sigFound && app.WebserverType == nodeps.WebserverNginxFPM {
		util.Warning("Using custom nginx configuration in %s", nginxFullConfigPath)
		customConfig = true
	}

	apacheFullConfigPath := app.GetConfigPath("apache/apache-site.conf")
	sigFound, _ = fileutil.FgrepStringInFile(apacheFullConfigPath, DdevFileSignature)
	if !sigFound && app.WebserverType != nodeps.WebserverNginxFPM {
		util.Warning("Using custom apache configuration in %s", apacheFullConfigPath)
		customConfig = true
	}

	nginxPath := filepath.Join(ddevDir, "nginx")
	if _, err := os.Stat(nginxPath); err == nil {
		nginxFiles, err := filepath.Glob(nginxPath + "/*.conf")
		util.CheckErr(err)
		if len(nginxFiles) > 0 {
			util.Warning("Using nginx snippets: %v", nginxFiles)
			customConfig = true
		}
	}

	mysqlPath := filepath.Join(ddevDir, "mysql")
	if _, err := os.Stat(mysqlPath); err == nil {
		mysqlFiles, err := filepath.Glob(mysqlPath + "/*.cnf")
		util.CheckErr(err)
		if len(mysqlFiles) > 0 {
			util.Warning("Using custom mysql configuration: %v", mysqlFiles)
			customConfig = true
		}
	}

	phpPath := filepath.Join(ddevDir, "php")
	if _, err := os.Stat(phpPath); err == nil {
		phpFiles, err := filepath.Glob(phpPath + "/*.ini")
		util.CheckErr(err)
		if len(phpFiles) > 0 {
			util.Warning("Using custom PHP configuration: %v", phpFiles)
			customConfig = true
		}
	}
	if customConfig {
		util.Warning("Custom configuration takes effect when container is created,\nusually on start, use 'ddev restart' if you're not seeing it take effect.")
	}

}

// CheckDeprecations warns the user if anything in use is deprecated.
func (app *DdevApp) CheckDeprecations() {

}

// FixObsolete removes files that may be obsolete, etc.
func (app *DdevApp) FixObsolete() {

	// Remove old in-project commands (which have been moved to global)
	for _, command := range []string{"db/mysql", "host/launch", "web/xdebug"} {
		cmdPath := app.GetConfigPath(filepath.Join("commands", command))
		signatureFound, err := fileutil.FgrepStringInFile(cmdPath, DdevFileSignature)
		if err == nil && signatureFound {
			err = os.Remove(cmdPath)
			if err != nil {
				util.Warning("attempted to remove %s but failed, you may want to remove it manually: %v", cmdPath, err)
			}
		}
	}
}

type composeYAMLVars struct {
	Name                      string
	Plugin                    string
	AppType                   string
	MailhogPort               string
	HostMailhogPort           string
	DBAPort                   string
	DBPort                    string
	HostPHPMyAdminPort        string
	DdevGenerated             string
	HostDockerInternalIP      string
	ComposeVersion            string
	DisableSettingsManagement bool
	MountType                 string
	WebMount                  string
	WebBuildContext           string
	DBBuildContext            string
	WebBuildDockerfile        string
	DBBuildDockerfile         string
	SSHAgentBuildContext      string
	OmitDB                    bool
	OmitDBA                   bool
	OmitRouter                bool
	OmitSSHAgent              bool
	BindAllInterfaces         bool
	MariaDBVolumeName         string
	MutagenEnabled            bool
	MutagenVolumeName         string
	NFSMountEnabled           bool
	NFSSource                 string
	NFSMountVolumeName        string
	DockerIP                  string
	IsWindowsFS               bool
	NoProjectMount            bool
	Hostnames                 []string
	Timezone                  string
	ComposerVersion           string
	Username                  string
	UID                       string
	GID                       string
	AutoRestartContainers     bool
	FailOnHookFail            bool
	WebWorkingDir             string
	DBWorkingDir              string
	DBAWorkingDir             string
	WebEnvironment            []string
	NoBindMounts              bool
	Docroot                   string
	ContainerUploadDir        string
	HostUploadDir             string
	GitDirMount               bool
}

// RenderComposeYAML renders the contents of .ddev/.ddev-docker-compose*.
func (app *DdevApp) RenderComposeYAML() (string, error) {
	var doc bytes.Buffer
	var err error

	hostDockerInternalIP, err := dockerutil.GetHostDockerInternalIP()
	if err != nil {
		util.Warning("Could not determine host.docker.internal IP address: %v", err)
	}
	// The fallthrough default for hostDockerInternalIdentifier is the
	// hostDockerInternalHostname == host.docker.internal

	webEnvironment := globalconfig.DdevGlobalConfig.WebEnvironment
	localWebEnvironment := app.WebEnvironment
	for _, v := range localWebEnvironment {
		// docker-compose won't accept a duplicate environment value
		if !nodeps.ArrayContainsString(webEnvironment, v) {
			webEnvironment = append(webEnvironment, v)
		}
	}

	uid, gid, username := util.GetContainerUIDGid()
	_, err = app.GetProvider("")
	if err != nil {
		return "", err
	}

	templateVars := composeYAMLVars{
		Name:                      app.Name,
		Plugin:                    "ddev",
		AppType:                   app.Type,
		MailhogPort:               GetPort("mailhog"),
		HostMailhogPort:           app.HostMailhogPort,
		DBAPort:                   GetPort("dba"),
		DBPort:                    GetPort("db"),
		HostPHPMyAdminPort:        app.HostPHPMyAdminPort,
		DdevGenerated:             DdevFileSignature,
		HostDockerInternalIP:      hostDockerInternalIP,
		ComposeVersion:            version.DockerComposeFileFormatVersion,
		DisableSettingsManagement: app.DisableSettingsManagement,
		OmitDB:                    nodeps.ArrayContainsString(app.GetOmittedContainers(), nodeps.DBContainer),
		OmitDBA:                   nodeps.ArrayContainsString(app.GetOmittedContainers(), nodeps.DBAContainer) || nodeps.ArrayContainsString(app.OmitContainers, nodeps.DBContainer),
		OmitRouter:                nodeps.ArrayContainsString(app.GetOmittedContainers(), globalconfig.DdevRouterContainer),
		OmitSSHAgent:              nodeps.ArrayContainsString(app.GetOmittedContainers(), "ddev-ssh-agent"),
		BindAllInterfaces:         app.BindAllInterfaces,
		MutagenEnabled:            app.IsMutagenEnabled() || globalconfig.DdevGlobalConfig.NoBindMounts,

		NFSMountEnabled:       (app.NFSMountEnabled || app.NFSMountEnabledGlobal) && !app.IsMutagenEnabled(),
		NFSSource:             "",
		IsWindowsFS:           runtime.GOOS == "windows",
		NoProjectMount:        app.NoProjectMount,
		MountType:             "bind",
		WebMount:              "../",
		Hostnames:             app.GetHostnames(),
		Timezone:              app.Timezone,
		ComposerVersion:       app.ComposerVersion,
		Username:              username,
		UID:                   uid,
		GID:                   gid,
		WebBuildContext:       "./web-build",
		DBBuildContext:        "./db-build",
		WebBuildDockerfile:    "../.webimageBuild/Dockerfile",
		DBBuildDockerfile:     "../.dbimageBuild/Dockerfile",
		AutoRestartContainers: globalconfig.DdevGlobalConfig.AutoRestartContainers,
		FailOnHookFail:        app.FailOnHookFail || app.FailOnHookFailGlobal,
		WebWorkingDir:         app.GetWorkingDir("web", ""),
		DBWorkingDir:          app.GetWorkingDir("db", ""),
		DBAWorkingDir:         app.GetWorkingDir("dba", ""),
		WebEnvironment:        webEnvironment,
		MariaDBVolumeName:     app.GetMariaDBVolumeName(),
		NFSMountVolumeName:    app.GetNFSMountVolumeName(),
		NoBindMounts:          globalconfig.DdevGlobalConfig.NoBindMounts,
		Docroot:               app.GetDocroot(),
		HostUploadDir:         app.GetHostUploadDirFullPath(),
		ContainerUploadDir:    app.GetContainerUploadDirFullPath(),
		GitDirMount:           false,
	}
	// We don't want to bind-mount git dir if it doesn't exist
	if fileutil.IsDirectory(filepath.Join(app.AppRoot, ".git")) {
		templateVars.GitDirMount = true
	}
	// And we don't want to bind-mount upload dir if it doesn't exist.
	// templateVars.UploadDir is relative path rooted in approot.
	if app.GetHostUploadDirFullPath() == "" || !fileutil.FileExists(app.GetHostUploadDirFullPath()) {
		templateVars.HostUploadDir = ""
		templateVars.ContainerUploadDir = ""
	}

	if app.NFSMountEnabled || app.NFSMountEnabledGlobal {
		templateVars.MountType = "volume"
		templateVars.WebMount = "nfsmount"
		templateVars.NFSSource = app.AppRoot
		// Workaround for Catalina sharing nfs as /System/Volumes/Data
		if runtime.GOOS == "darwin" && fileutil.IsDirectory(filepath.Join("/System/Volumes/Data", app.AppRoot)) {
			templateVars.NFSSource = filepath.Join("/System/Volumes/Data", app.AppRoot)
		}
		if runtime.GOOS == "windows" {
			// WinNFSD can only handle a mountpoint like /C/Users/rfay/workspace/d8git
			// and completely chokes in C:\Users\rfay...
			templateVars.NFSSource = dockerutil.MassageWindowsNFSMount(app.AppRoot)
		}
	}

	if app.IsMutagenEnabled() {
		templateVars.MutagenVolumeName = GetMutagenVolumeName(app)
	}

	// Add web and db extra dockerfile info
	// If there is a user-provided Dockerfile, use that as the base and then add
	// our extra stuff like usernames, etc.
	// The db-build and web-build directories are used for context
	// so must exist. They usually do.
	err = os.MkdirAll(app.GetConfigPath("db-build"), 0755)
	if err != nil {
		return "", err
	}

	err = os.MkdirAll(app.GetConfigPath("web-build"), 0755)
	if err != nil {
		return "", err
	}

	err = WriteBuildDockerfile(app.GetConfigPath(".webimageBuild/Dockerfile"), app.GetConfigPath("web-build/Dockerfile"), app.WebImageExtraPackages, app.ComposerVersion)
	if err != nil {
		return "", err
	}

	err = WriteBuildDockerfile(app.GetConfigPath(".dbimageBuild/Dockerfile"), app.GetConfigPath("db-build/Dockerfile"), app.DBImageExtraPackages, "")

	if err != nil {
		return "", err
	}

	// SSH agent just needs extra to add the official related user, nothing else
	err = WriteBuildDockerfile(filepath.Join(globalconfig.GetGlobalDdevDir(), ".sshimageBuild/Dockerfile"), "", nil, "")
	if err != nil {
		return "", err
	}

	templateVars.DockerIP, err = dockerutil.GetDockerIP()
	if err != nil {
		return "", err
	}
	if app.BindAllInterfaces {
		templateVars.DockerIP = "0.0.0.0"
	}

	t, err := template.New("app_compose_template.yaml").Funcs(sprig.TxtFuncMap()).ParseFS(bundledAssets, "app_compose_template.yaml")
	if err != nil {
		return "", err
	}

	err = t.Execute(&doc, templateVars)
	return doc.String(), err
}

// WriteBuildDockerfile writes a Dockerfile to be used in the
// docker-compose 'build'
// It may include the contents of .ddev/<container>-build
func WriteBuildDockerfile(fullpath string, userDockerfile string, extraPackages []string, composerVersion string) error {
	// Start with user-built dockerfile if there is one.
	err := os.MkdirAll(filepath.Dir(fullpath), 0755)
	if err != nil {
		return err
	}

	// Normal starting content is just the arg and base image
	contents := `
ARG BASE_IMAGE
FROM $BASE_IMAGE
`
	// If there is a user dockerfile, start with its contents
	if userDockerfile != "" && fileutil.FileExists(userDockerfile) {
		contents, err = fileutil.ReadFileIntoString(userDockerfile)
		if err != nil {
			return err
		}
	}
	contents = contents + `
ARG username
ARG uid
ARG gid
RUN (groupadd --gid $gid "$username" || groupadd "$username" || true) && (useradd  -l -m -s "/bin/bash" --gid "$username" --comment '' --uid $uid "$username" || useradd  -l -m -s "/bin/bash" --gid "$username" --comment '' "$username" || useradd  -l -m -s "/bin/bash" --gid "$gid" --comment '' "$username")
`
	if extraPackages != nil {
		contents = contents + `
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y -o Dpkg::Options::="--force-confold" --no-install-recommends --no-install-suggests ` + strings.Join(extraPackages, " ") + "\n"
	}

	// For webimage, update to latest composer.
	if strings.Contains(fullpath, "webimageBuild") {
		// If composerVersion is set,
		// run composer self-update to the version (or --1 or --2)
		// defaults to "2" even if ""
		var composerSelfUpdateArg string
		switch composerVersion {
		case "1":
			composerSelfUpdateArg = "--1"
		case "":
			fallthrough
		case "2":
			composerSelfUpdateArg = "--2"
		default:
			composerSelfUpdateArg = composerVersion
		}

		// Composer v2 is default
		// Try composer self-update twice because of troubles with composer downloads
		// breaking testing.
		contents = contents + fmt.Sprintf(`
RUN export XDEBUG_MODE=off && ( composer self-update %s || composer self-update %s || true )
`, composerSelfUpdateArg, composerSelfUpdateArg)
	}
	return WriteImageDockerfile(fullpath, []byte(contents))
}

// WriteImageDockerfile writes a dockerfile at the fullpath (including the filename)
func WriteImageDockerfile(fullpath string, contents []byte) error {
	err := os.MkdirAll(filepath.Dir(fullpath), 0755)
	if err != nil {
		return err
	}
	err = os.WriteFile(fullpath, contents, 0644)
	if err != nil {
		return err
	}
	return nil
}

// prompt for a project name.
func (app *DdevApp) promptForName() error {
	if app.Name == "" {
		dir, err := os.Getwd()
		// if working directory name is invalid for hostnames, we shouldn't suggest it
		if err == nil && hostRegex.MatchString(filepath.Base(dir)) {
			app.Name = filepath.Base(dir)
		}
	}

	name := util.Prompt("Project name", app.Name)
	if err := ValidateProjectName(name); err != nil {
		return err
	}
	app.Name = name
	return nil
}

// AvailableDocrootLocations returns an of default docroot locations to look for.
func AvailableDocrootLocations() []string {
	return []string{
		"_www",
		"docroot",
		"htdocs",
		"html",
		"pub",
		"public",
		"web",
		"web/public",
		"webroot",
	}
}

// DiscoverDefaultDocroot returns the default docroot directory.
func DiscoverDefaultDocroot(app *DdevApp) string {
	// Provide use the app.Docroot as the default docroot option.
	var defaultDocroot = app.Docroot
	if defaultDocroot == "" {
		for _, docroot := range AvailableDocrootLocations() {
			if _, err := os.Stat(filepath.Join(app.AppRoot, docroot)); err != nil {
				continue
			}

			if fileutil.FileExists(filepath.Join(app.AppRoot, docroot, "index.php")) {
				defaultDocroot = docroot
				break
			}
		}
	}
	return defaultDocroot
}

// Determine the document root.
func (app *DdevApp) docrootPrompt() error {

	// Determine the document root.
	util.Warning("\nThe docroot is the directory from which your site is served.\nThis is a relative path from your project root at %s", app.AppRoot)
	output.UserOut.Println("You may leave this value blank if your site files are in the project root")
	var docrootPrompt = "Docroot Location"
	var defaultDocroot = DiscoverDefaultDocroot(app)
	// If there is a default docroot, display it in the prompt.
	if defaultDocroot != "" {
		docrootPrompt = fmt.Sprintf("%s (%s)", docrootPrompt, defaultDocroot)
	} else if cd, _ := os.Getwd(); cd == filepath.Join(app.AppRoot, defaultDocroot) {
		// Preserve the case where the docroot is the current directory
		docrootPrompt = fmt.Sprintf("%s (current directory)", docrootPrompt)
	} else {
		// Explicitly state 'project root' when in a subdirectory
		docrootPrompt = fmt.Sprintf("%s (project root)", docrootPrompt)
	}

	fmt.Print(docrootPrompt + ": ")
	app.Docroot = util.GetInput(defaultDocroot)

	// Ensure the docroot exists. If it doesn't, prompt the user to verify they entered it correctly.
	fullPath := filepath.Join(app.AppRoot, app.Docroot)
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		util.Warning("Warning: the provided docroot at %s does not currently exist.", fullPath)

		// Ask the user for permission to create the docroot
		if !util.Confirm(fmt.Sprintf("Create docroot at %s?", fullPath)) {
			return fmt.Errorf("docroot must exist to continue configuration")
		}

		if err = os.MkdirAll(fullPath, 0755); err != nil {
			return fmt.Errorf("unable to create docroot: %v", err)
		}

		util.Success("Created docroot at %s.", fullPath)
	}

	return nil
}

// ConfigExists determines if a ddev config file exists for this application.
func (app *DdevApp) ConfigExists() bool {
	if _, err := os.Stat(app.ConfigPath); os.IsNotExist(err) {
		return false
	}
	return true
}

// AppTypePrompt handles the Type workflow.
func (app *DdevApp) AppTypePrompt() error {
	validAppTypes := strings.Join(GetValidAppTypes(), ", ")
	typePrompt := fmt.Sprintf("Project Type [%s]", validAppTypes)

	// First, see if we can auto detect what kind of site it is so we can set a sane default.

	detectedAppType := app.DetectAppType()
	// If the detected detectedAppType is php, we'll ask them to confirm,
	// otherwise go with it.
	// If we found an application type just set it and inform the user.
	util.Success("Found a %s codebase at %s.", detectedAppType, filepath.Join(app.AppRoot, app.Docroot))
	typePrompt = fmt.Sprintf("%s (%s)", typePrompt, detectedAppType)

	fmt.Printf(typePrompt + ": ")
	appType := strings.ToLower(util.GetInput(detectedAppType))

	for !IsValidAppType(appType) {
		output.UserOut.Errorf("'%s' is not a valid project type. Allowed project types are: %s\n", appType, validAppTypes)

		fmt.Printf(typePrompt + ": ")
		appType = strings.ToLower(util.GetInput(appType))
	}
	app.Type = appType
	return nil
}

// PrepDdevDirectory creates a .ddev directory in the current working directory
func PrepDdevDirectory(dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {

		log.WithFields(log.Fields{
			"directory": dir,
		}).Debug("Config Directory does not exist, attempting to create.")

		err := os.MkdirAll(dir, 0755)
		if err != nil {
			return err
		}
	}

	err := CreateGitIgnore(dir, "**/*.example", ".dbimageBuild", ".dbimageExtra", ".ddev-docker-*.yaml", ".*downloads", ".global_commands", ".homeadditions", ".sshimageBuild", ".webimageBuild", ".webimageExtra", "apache/apache-site.conf", "commands/.gitattributes", "commands/db/mysql", "commands/host/launch", "commands/web/xdebug", "commands/web/live", "config.*.y*ml", "db_snapshots", "import-db", "import.yaml", "mutagen", "nginx_full/nginx-site.conf", "sequelpro.spf", "xhprof", "**/README.*")
	if err != nil {
		return fmt.Errorf("failed to create gitignore in %s: %v", dir, err)
	}

	return nil
}

// validateHookYAML validates command hooks and tasks defined in hooks for config.yaml
func validateHookYAML(source []byte) error {
	validHooks := []string{
		"pre-start",
		"post-start",
		"pre-import-db",
		"post-import-db",
		"pre-import-files",
		"post-import-files",
		"pre-composer",
		"post-composer",
		"pre-stop",
		"post-stop",
		"pre-config",
		"post-config",
		"pre-describe",
		"post-describe",
		"pre-exec",
		"post-exec",
		"pre-pause",
		"post-pause",
		"pre-pull",
		"post-pull",
		"pre-push",
		"post-push",
		"pre-snapshot",
		"post-snapshot",
		"pre-restore-snapshot",
		"post-restore-snapshot",
	}

	validTasks := []string{
		"exec",
		"exec-host",
		"composer",
	}

	type Validate struct {
		Commands map[string][]map[string]interface{} `yaml:"hooks,omitempty"`
	}
	val := &Validate{}

	err := yaml.Unmarshal(source, val)
	if err != nil {
		return err
	}

	for foundHook, tasks := range val.Commands {
		var match bool
		for _, h := range validHooks {
			if foundHook == h {
				match = true
			}
		}
		if !match {
			return fmt.Errorf("invalid hook %s defined in config.yaml", foundHook)
		}

		for _, foundTask := range tasks {
			var match bool
			for _, validTaskName := range validTasks {
				if _, ok := foundTask[validTaskName]; ok {
					match = true
				}
			}
			if !match {
				return fmt.Errorf("invalid task '%s' defined for hook %s in config.yaml", foundTask, foundHook)
			}

		}

	}

	return nil
}
