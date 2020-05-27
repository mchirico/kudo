package diagnostics

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/afero"
	"gopkg.in/yaml.v2"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/kudobuilder/kudo/pkg/kudoctl/util/kudo"
)

const (
	DiagDir = "diag"
	KudoDir = "diag/kudo"
)

type printMode string

const (
	ObjectWithDir      printMode = "ObjectsWithDir"
	ObjectListWithDirs printMode = "ObjectListWithDirs" // print each object into its own nested directory based on its name and kind
	RuntimeObject      printMode = "RuntimeObject"      // print as a file based on its kind only
)

// nonFailingPrinter - print provided data into provided directory and accumulate errors instead of returning them.
// Creates a nested directory if an object type requires so.
type nonFailingPrinter struct {
	fs     afero.Fs
	errors []string
}

func (p *nonFailingPrinter) printObject(obj runtime.Object, parentDir string, mode printMode) {
	switch mode {
	case ObjectWithDir:
		if err := printSingleObject(p.fs, obj, parentDir); err != nil {
			p.errors = append(p.errors, err.Error())
		}
	case ObjectListWithDirs:
		err := meta.EachListItem(obj, func(object runtime.Object) error {
			if err := printSingleObject(p.fs, object, parentDir); err != nil {
				p.errors = append(p.errors, err.Error())
			}
			return nil
		})
		if err != nil {
			p.errors = append(p.errors, err.Error())
		}
	case RuntimeObject:
		fallthrough
	default:
		if err := printSingleRuntimeObject(p.fs, obj, parentDir); err != nil {
			p.errors = append(p.errors, err.Error())
		}
	}
}

func (p *nonFailingPrinter) printError(err error, parentDir, name string) {
	b := []byte(err.Error())
	if err := printBytes(p.fs, b, parentDir, fmt.Sprintf("%s.err", name)); err != nil {
		p.errors = append(p.errors, err.Error())
	}
}

func (p *nonFailingPrinter) printLog(log io.ReadCloser, parentDir, name string) {
	if err := printLog(p.fs, log, parentDir, name); err != nil {
		p.errors = append(p.errors, err.Error())
	}
}

func (p *nonFailingPrinter) printYaml(v interface{}, parentDir, name string) {
	if err := printYaml(p.fs, v, parentDir, name); err != nil {
		p.errors = append(p.errors, err.Error())
	}
}

// printSingleObject - print a runtime.object assuming it exposes metadata by implementing metav1.object
// or panic otherwise. object is put into a nested directory.
func printSingleObject(fs afero.Fs, obj runtime.Object, parentDir string) error {
	if !isKudoCR(obj) {
		err := kudo.SetGVKFromScheme(obj, scheme.Scheme)
		if err != nil {
			return err
		}
	}

	o, ok := obj.(metav1.Object)
	if !ok {
		return fmt.Errorf("invalid print mode: can't get name for %s", strings.ToLower(obj.GetObjectKind().GroupVersionKind().Kind))
	}

	relToParentDir := fmt.Sprintf("%s_%s", strings.ToLower(obj.GetObjectKind().GroupVersionKind().Kind), o.GetName())
	dir := filepath.Join(parentDir, relToParentDir)
	name := fmt.Sprintf("%s.yaml", o.GetName())
	return printRuntimeObjectInto(fs, obj, dir, name)
}

func printRuntimeObjectInto(fs afero.Fs, obj runtime.Object, dir, name string) error {
	err := fs.MkdirAll(dir, 0700)
	if err != nil {
		return fmt.Errorf("failed to create directory %s: %v", dir, err)
	}

	fileWithPath := filepath.Join(dir, name)
	file, err := fs.Create(fileWithPath)
	if err != nil {
		return fmt.Errorf("failed to create %s: %v", fileWithPath, err)
	}
	defer file.Close()

	printer := printers.YAMLPrinter{}
	return printer.PrintObj(obj, file)
}

// printSingleRuntimeObject - print a runtime.Object in the supplied dir.
func printSingleRuntimeObject(fs afero.Fs, obj runtime.Object, dir string) error {
	err := kudo.SetGVKFromScheme(obj, scheme.Scheme)
	if err != nil {
		return err
	}

	name := fmt.Sprintf("%s.yaml", strings.ToLower(obj.GetObjectKind().GroupVersionKind().Kind))
	return printRuntimeObjectInto(fs, obj, dir, name)
}

func printLog(fs afero.Fs, log io.ReadCloser, dir, podName string) error {
	err := fs.MkdirAll(dir, 0700)
	if err != nil {
		return fmt.Errorf("failed to create directory %s: %v", dir, err)
	}

	fileNameWithPath := filepath.Join(dir, fmt.Sprintf("%s.log.gz", podName))
	file, err := fs.Create(fileNameWithPath)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %v", fileNameWithPath, err)
	}
	defer file.Close()

	z := newGzipWriter(file)
	err = z.write(log)
	if err != nil {
		return fmt.Errorf("failed to write to file %s: %v", fileNameWithPath, err)
	}
	return nil
}

func printYaml(fs afero.Fs, v interface{}, dir, name string) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Errorf("failed to marshal object to %s/%s.yaml: %v", dir, name, err)
	}

	err = fs.MkdirAll(dir, 0700)
	if err != nil {
		return fmt.Errorf("failed to create directory %s: %v", dir, err)
	}

	name = fmt.Sprintf("%s.yaml", name)
	return printBytes(fs, b, dir, name)
}

func printBytes(fs afero.Fs, b []byte, dir, name string) error {
	err := fs.MkdirAll(dir, 0700)
	if err != nil {
		return fmt.Errorf("failed to create directory %s: %v", dir, err)
	}

	fileNameWithPath := filepath.Join(dir, name)
	file, err := fs.Create(fileNameWithPath)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %v", fileNameWithPath, err)
	}
	defer file.Close()

	_, err = file.Write(b)
	if err != nil {
		return fmt.Errorf("failed to write to file %s: %v", fileNameWithPath, err)
	}
	return nil
}

func isKudoCR(obj runtime.Object) bool {
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	return kind == "Instance" || kind == "Operator" || kind == "OperatorVersion"
}