package gotexttemplate

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"text/template"
	"time"

	"github.com/avanha/pmaas-spi"
)

var _defaultTemplateModTime = time.Unix(0, 0)

type state struct {
	config     GoLangTextTemplatePluginConfig
	container  spi.IPMAASContainer
	cache      map[string]templateLoadState
	cacheMutex sync.Mutex
}

type goLangTextTemplatePlugin struct {
	state *state
}

type GoLangTextTemplatePlugin interface {
	spi.IPMAASTemplateEnginePlugin
}

type goLangTextTemplateWrapper struct {
	template     *template.Template
	templateName string
	fileModTimes map[string]time.Time
}

// A function that returns the result of a template load operation.
type templateProvider func() (goLangTextTemplateWrapper, error)

// A function that consumes the result of a template load operation.
type templateResultConsumer func(goLangTextTemplateWrapper, error)

type templateLoadState struct {
	provider     templateProvider
	loadedTime   time.Time
	loadComplete bool
}

func NewGoLangTextTemplatePlugin(config GoLangTextTemplatePluginConfig) GoLangTextTemplatePlugin {
	instance := &goLangTextTemplatePlugin{
		state: &state{
			config:    config,
			container: nil,
			cache:     make(map[string]templateLoadState),
		},
	}

	return instance
}

// Implementation of spi.IPMAASPlugin
var _ spi.IPMAASTemplateEnginePlugin = (*goLangTextTemplatePlugin)(nil)

func (p *goLangTextTemplatePlugin) Init(container spi.IPMAASContainer) {
	p.state.container = container
}

func (p *goLangTextTemplatePlugin) Start() {
	fmt.Printf("%s Starting...\n", *p)
}

func (p *goLangTextTemplatePlugin) Stop() {
	fmt.Printf("%s Stopping...\n", *p)
}

func (p *goLangTextTemplatePlugin) GetTemplate(templateInfo *spi.TemplateInfo) (spi.CompiledTemplate, error) {
	startTime := time.Now()
	templateInstance, err := p.getTemplateProvider(templateInfo)()

	if err != nil {
		fmt.Printf("GetTemplate completing with error after %v\n", time.Now().Sub(startTime))
		return spi.CompiledTemplate{}, err
	}

	//fmt.Printf("GetTemplate completing successfully after %v\n", time.Now().Sub(startTime))
	return spi.CompiledTemplate{
		Instance: &templateInstance,
		Scripts:  templateInfo.Scripts,
		Styles:   templateInfo.Styles,
	}, nil
}

// Returns a no-arg template provider function that returns an instance of goLangTextTemplateWrapper.
// goLangTextTemplateWrapper implements the spi.ITemplate interface.  This thread-safe function maintains the cache of
// loaded templates and optimizes further by replacing the provider obtained from createTemplateProvider with one that
// simply returns a previously loaded value, avoiding synchronization within the loader.
func (p *goLangTextTemplatePlugin) getTemplateProvider(templateInfo *spi.TemplateInfo) templateProvider {
	p.state.cacheMutex.Lock()
	defer p.state.cacheMutex.Unlock()

	loadState, ok := p.state.cache[templateInfo.Name]
	now := time.Now()
	loadNeeded := false

	if ok {
		age := now.Sub(loadState.loadedTime)
		loadNeeded = age >= p.state.config.templateCacheDuration
		//fmt.Printf("Provider for template %s is in cache, age is %v, config.templateCacheDuration is %v, loadNeeded: %v\n",
		//	templateInfo.Name, age, p.state.config.templateCacheDuration, loadNeeded)
	} else {
		loadNeeded = true
		//fmt.Printf("Provider for template %s is NOT in cache, loadNeeded: %v\n", templateInfo.Name, loadNeeded)
	}

	if loadNeeded {
		onTemplateLoadDone := func(template goLangTextTemplateWrapper, err error) {
			// When the template is actually loaded, asynchronously replace the loading provider with one that returns
			// the given result. This saves the overhead of sync.Once for future calls.
			go func() {
				p.state.cacheMutex.Lock()
				defer p.state.cacheMutex.Unlock()

				optimizedProvider := func() (goLangTextTemplateWrapper, error) {
					return template, err
				}

				p.state.cache[templateInfo.Name] = templateLoadState{
					provider:     optimizedProvider,
					loadedTime:   now,
					loadComplete: true,
				}
			}()
		}
		var currentTemplateWrapper goLangTextTemplateWrapper

		if loadState.loadComplete {
			tw, err := loadState.provider()

			if err == nil {
				currentTemplateWrapper = tw
			}
		}

		provider := p.createTemplateProvider(templateInfo, currentTemplateWrapper, onTemplateLoadDone)
		loadState = templateLoadState{
			provider:     provider,
			loadedTime:   now,
			loadComplete: false,
		}
		p.state.cache[templateInfo.Name] = loadState
	}

	return loadState.provider
}

func (p *goLangTextTemplatePlugin) createTemplateProvider(
	templateInfo *spi.TemplateInfo,
	currentTemplate goLangTextTemplateWrapper,
	onTemplateLoadDone templateResultConsumer) templateProvider {
	var once sync.Once
	var newTemplate goLangTextTemplateWrapper

	// Initialize err with a value.  If loadTemplate completes normally, it will give us a new value.  If it panics,
	// we'll end up using this value.
	var err = errors.New(fmt.Sprintf("unable to load template \"%s\", loadTemplate function did not "+
		"return a result", templateInfo.Name))
	return func() (goLangTextTemplateWrapper, error) {
		// Loads the template and saves the result (template or error) into local variables.
		// The mutex within once.Do() ensures memory synchronization, so any go routines that execute this function are
		// guaranteed to see the results after once.Do() completes.  We also execute onTemplateLoadDone, to let the
		// consumer know when the one-time load completes.
		// See https://notes.shichao.io/gopl/ch9/#lazy-initialization-synconce
		once.Do(func() {
			newTemplate, err = p.loadTemplate(templateInfo, currentTemplate)
			onTemplateLoadDone(newTemplate, err)
		})

		return newTemplate, err
	}
}

func (p *goLangTextTemplatePlugin) loadTemplate(
	templateInfo *spi.TemplateInfo,
	currentTemplate goLangTextTemplateWrapper) (goLangTextTemplateWrapper, error) {
	//fmt.Printf("Loading template \"%s\" from \"%s\" (Full set of paths: %v)\n", templateInfo.Name, templateInfo.Paths[0], templateInfo.Paths)
	fileModTimes, err := getFileModTimes(templateInfo.Paths, templateInfo.SourceFS)

	if err != nil {
		return goLangTextTemplateWrapper{}, fmt.Errorf("unable to load template \"%s\": %w", templateInfo.Name, err)
	}

	if isUpToDate(fileModTimes, currentTemplate) {
		//fmt.Printf("Template \"%s\" input files unchanged, reusing current instance\n", templateInfo.Name)
		return currentTemplate, nil
	}

	rootTemplate, err := template.New("unused_root").Funcs(templateInfo.FuncMap).ParseFS(
		templateInfo.SourceFS, templateInfo.Paths...)

	if err != nil {
		return goLangTextTemplateWrapper{}, err
	}

	childTemplates := rootTemplate.Templates()

	if len(childTemplates) == 0 {
		return goLangTextTemplateWrapper{}, errors.New("parse of template files did not create any new template instances")
	}

	firstChildTemplate := childTemplates[0]

	result := goLangTextTemplateWrapper{
		template:     firstChildTemplate,
		templateName: templateInfo.Name,
		fileModTimes: fileModTimes,
	}

	return result, nil
}

func getFileModTimes(paths []string, sourceFS fs.FS) (map[string]time.Time, error) {
	result := make(map[string]time.Time)

	// Not all sources (i.e. embedded) support the Stat method.
	sourceStatFS, ok := sourceFS.(fs.StatFS)

	if !ok {
		for _, path := range paths {
			result[path] = _defaultTemplateModTime
		}

		return result, nil
	}

	for _, path := range paths {
		fileInfo, err := sourceStatFS.Stat(path)

		if err != nil {
			return nil, fmt.Errorf("unable to stat file \"%s\": %w", path, err)
		}

		result[path] = fileInfo.ModTime()
	}

	return result, nil
}

func isUpToDate(fileModTimes map[string]time.Time, currentTemplate goLangTextTemplateWrapper) bool {
	if currentTemplate.template == nil {
		return false
	}

	for path, modTime := range fileModTimes {
		if modTime.After(currentTemplate.fileModTimes[path]) {
			return false
		}
	}

	return true
}

// Implementation of spi.ITemplate
var _ spi.ITemplate = (*goLangTextTemplateWrapper)(nil)

func (t *goLangTextTemplateWrapper) Execute(wr io.Writer, data any) error {
	return t.template.Execute(wr, data)
}
