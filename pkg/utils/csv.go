package utils

import (
	"fmt"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/blang/semver"
	olmapiv1alpha1 "github.com/operator-framework/operator-lifecycle-manager/pkg/api/apis/operators/v1alpha1"
	"github.com/operator-framework/operator-registry/pkg/registry"
)

var ReProd = regexp.MustCompile(`registry.redhat.io/.*`)
var ReStage = regexp.MustCompile(`registry.stage.redhat.io/.*`)
var ReProxy = regexp.MustCompile(`registry-proxy.engineering.redhat.com/rh-osbs/.*`)
var ReDelorean = regexp.MustCompile(`quay.io/integreatly/delorean.*`)
var imageWhitelist = [1]string{"registry.redhat.io/openshift4/ose-oauth-proxy:4.2"}

const (
	imagePullSecret = "integreatly-delorean-pull-secret"
)

type csvName struct {
	Name    string
	Version semver.Version
}
type csvNames []csvName

func (c csvNames) Len() int           { return len(c) }
func (c csvNames) Less(i, j int) bool { return c[i].Version.LT(c[j].Version) }
func (c csvNames) Swap(i, j int)      { c[i], c[j] = c[j], c[i] }

// ReadCSVFromBundleDirectory tries to parse every YAML file in the directory and see if they are CSV.
// According to the strict one CSV rule for every bundle, we return the first file that is considered a CSV type.
func ReadCSVFromBundleDirectory(bundleDir string) (*olmapiv1alpha1.ClusterServiceVersion, string, error) {
	dirContent, err := ioutil.ReadDir(bundleDir)
	if err != nil {
		return nil, "", fmt.Errorf("error reading bundle directory %s, %v", bundleDir, err)
	}

	files := []string{}
	for _, f := range dirContent {
		if !f.IsDir() {
			files = append(files, f.Name())
		}
	}

	for _, file := range files {
		if strings.Contains(file, ".clusterserviceversion.yaml") || strings.Contains(file, ".csv.yaml") {
			bundleFilepath := path.Join(bundleDir, file)
			var csv *olmapiv1alpha1.ClusterServiceVersion
			err := PopulateObjectFromYAML(bundleFilepath, &csv)
			if err != nil {
				return nil, "", err
			}
			return csv, bundleFilepath, nil
		}
	}
	return nil, "", fmt.Errorf("no ClusterServiceVersion object found in %s", bundleDir)

}

func VerifyManifestDirs(dirs ...string) error {
	for _, dir := range dirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return err
		}

		matches, err := filepath.Glob(dir + "/*.package.yaml")
		if err != nil {
			return err
		}

		if len(matches) == 0 {
			return fmt.Errorf("No package.yaml file found in %s", dir)
		}
	}
	return nil
}

func GetPackageManifest(packageDir string) (*registry.PackageManifest, string, error) {
	matches, err := filepath.Glob(packageDir + "/*.package.yaml")
	if err != nil {
		return nil, "", err
	}

	if len(matches) == 0 {
		return nil, "", fmt.Errorf("No package.yaml file found in %s", packageDir)
	}

	var pkgManifestFile = matches[0]

	pkgManifest := &registry.PackageManifest{}
	if err = PopulateObjectFromYAML(pkgManifestFile, &pkgManifest); err != nil {
		return nil, "", err
	}

	return pkgManifest, pkgManifestFile, nil
}

func GetSortedCSVNames(packageDir string) (csvNames, error) {
	bundleDirs, err := ioutil.ReadDir(packageDir)
	var sortedCSVNames csvNames
	if err != nil {
		return nil, err
	}
	for _, bundlePath := range bundleDirs {
		if bundlePath.IsDir() {
			csv, _, err := ReadCSVFromBundleDirectory(filepath.Join(packageDir, bundlePath.Name()))
			if err != nil {
				return nil, err
			}
			sortedCSVNames = append(sortedCSVNames, csvName{Name: csv.Name, Version: csv.Spec.Version.Version})
		}
	}
	sort.Sort(sortedCSVNames)
	return sortedCSVNames, nil
}

func GetCurrentCSV(packageDir string) (*olmapiv1alpha1.ClusterServiceVersion, string, error) {

	pkgManifest, _, err := GetPackageManifest(packageDir)
	if err != nil {
		return nil, "", err
	}

	var currentCSVName string
	for _, channel := range pkgManifest.Channels {
		if channel.IsDefaultChannel(*pkgManifest) {
			currentCSVName = channel.CurrentCSVName
			break
		}
	}

	bundleDirs, err := ioutil.ReadDir(packageDir)
	if err != nil {
		return nil, "", fmt.Errorf("error reading from %s directory, %v", packageDir, err)
	}
	for _, bundlePath := range bundleDirs {
		if bundlePath.IsDir() {
			bundleDir := filepath.Join(packageDir, bundlePath.Name())
			csv, csvFile, err := ReadCSVFromBundleDirectory(bundleDir)
			if err != nil {
				return nil, "", err
			}
			if csv.Name == currentCSVName {
				return csv, csvFile, nil
			}
		}
	}

	return nil, "", fmt.Errorf("failed to find current csv in %s", packageDir)
}

func UpdatePackageManifest(packageDir, currentCSVName string) (*registry.PackageManifest, error) {

	pkgManifest, pkgManifestFile, err := GetPackageManifest(packageDir)
	if err != nil {
		return nil, err
	}

	pkgManifest.Channels[0].CurrentCSVName = fmt.Sprintf(currentCSVName)
	pkgManifest.DefaultChannelName = pkgManifest.Channels[0].Name

	err = WriteObjectToYAML(pkgManifest, pkgManifestFile)
	if err != nil {
		return nil, err
	}

	return pkgManifest, nil
}

func ProcessCurrentCSV(packageDir string, processFunc process) error {
	csv, csvfile, err := GetCurrentCSV(packageDir)
	if err != nil {
		return err
	}

	if processFunc != nil {
		err = processFunc(csv)
		if err != nil {
			return err
		}
	}

	err = WriteK8sObjectToYAML(csv, csvfile)
	if err != nil {
		return err
	}
	return nil
}

type process func(*olmapiv1alpha1.ClusterServiceVersion) error

func GetAndUpdateOperandImagesToDeloreanImages(manifestDir string, extraImages []string) ([]string, error) {
	csv, fp, err := GetCurrentCSV(manifestDir)
	if err != nil {
		return nil, err
	}

	if len(extraImages) > 0 {
		csv, err = includeImages(extraImages, csv)
		if err != nil {
			return nil, err
		}
	}

	var images []string
	deployment := csv.Spec.InstallStrategy.StrategySpec.DeploymentSpecs[0]
	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			var deloreanImage string
			var matched string
			prodMatched := ReProd.FindString(env.Value)
			//found a registry.redhat.io image
			if prodMatched != "" {
				matched = prodMatched
			}

			stageMatched := ReStage.FindString(env.Value)
			//found a registry.stage.redhat.io image
			if stageMatched != "" {
				matched = stageMatched
			}

			deloreanMatched := ReDelorean.FindString(env.Value)
			//found a quay.io/integreatly/delorean image so ignore
			if deloreanMatched != "" {
				continue
			}
			if matched != "" {
				deloreanImage = BuildDeloreanImage(matched)
				deloreanImage = StripSHAOrTag(deloreanImage)
				mirrorString := BuildOSBSImage(matched) + " " + deloreanImage
				images = append(images, mirrorString)
				container.Env = AddOrUpdateEnvVar(container.Env, env.Name, deloreanImage)
			}
		}
	}

	err = WriteObjectToYAML(csv, fp)
	if err != nil {
		return images, err
	}
	return images, nil
}

func includeImages(extraImages []string, csv *olmapiv1alpha1.ClusterServiceVersion) (*olmapiv1alpha1.ClusterServiceVersion, error) {
	deployment := csv.Spec.InstallStrategy.StrategySpec.DeploymentSpecs[0]
	for n, container := range deployment.Spec.Template.Spec.Containers {
		for _, i := range extraImages {
			currImage := strings.Split(i, "=")
			container.Env = AddOrUpdateEnvVar(container.Env, currImage[0], currImage[1])
		}
		deployment.Spec.Template.Spec.Containers[n] = container
	}

	return csv, nil
}

func UpdateOperatorImagesToDeloreanImages(manifestDir string, images []string) ([]string, error) {
	csv, fp, err := GetCurrentCSV(manifestDir)
	if err != nil {
		return nil, err
	}

	deployment := csv.Spec.InstallStrategy.StrategySpec.DeploymentSpecs[0]
	matched := ReDelorean.FindString(deployment.Spec.Template.Spec.Containers[0].Image)
	operatorImage := BuildDeloreanImage(deployment.Spec.Template.Spec.Containers[0].Image)
	if matched == "" {
		operatorImage = StripSHAOrTag(operatorImage)
		mirrorString := BuildOSBSImage(deployment.Spec.Template.Spec.Containers[0].Image) + " " + operatorImage
		images = append(images, mirrorString)
		deployment.Spec.Template.Spec.Containers[0].Image = operatorImage
	}

	annotationMatched := ReDelorean.FindString(csv.Annotations["containerImage"])
	if annotationMatched == "" {
		csv.Annotations["containerImage"] = operatorImage
	}

	err = WriteObjectToYAML(csv, fp)
	return images, nil
}

func GetOperandImages(manifestDir string) ([]string, error) {
	csv, _, err := GetCurrentCSV(manifestDir)
	if err != nil {
		return nil, err
	}

	var images []string
	deployment := csv.Spec.InstallStrategy.StrategySpec.DeploymentSpecs[0]
	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			matched := ReProd.FindString(env.Value)
			if matched == "" {
				matched = ReStage.FindString(env.Value)
			}
			if matched != "" {
				images = append(images, env.Value)
			}
		}
	}

	return images, nil
}

func FindDeploymentByName(deployments []olmapiv1alpha1.StrategyDeploymentSpec, name string) (int, *olmapiv1alpha1.StrategyDeploymentSpec) {
	for i, d := range deployments {
		if d.Name == name {
			return i, &d
		}
	}
	return -1, nil
}

func FindContainerByName(containers []corev1.Container, containerName string) (int, *corev1.Container) {
	for i, c := range containers {
		if c.Name == containerName {
			return i, &c
		}
	}
	return -1, nil
}

func FindInstallMode(installModes []olmapiv1alpha1.InstallMode, typeName olmapiv1alpha1.InstallModeType) (int, *olmapiv1alpha1.InstallMode) {
	for i, m := range installModes {
		if m.Type == typeName {
			return i, &m
		}
	}
	return -1, nil
}
