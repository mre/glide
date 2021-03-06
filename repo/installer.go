package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Masterminds/glide/cfg"
	"github.com/Masterminds/glide/dependency"
	"github.com/Masterminds/glide/importer"
	"github.com/Masterminds/glide/msg"
	gpath "github.com/Masterminds/glide/path"
	"github.com/Masterminds/glide/util"
	"github.com/Masterminds/semver"
	"github.com/codegangsta/cli"
)

// Installer provides facilities for installing the repos in a config file.
type Installer struct {

	// Force the install when certain normally stopping conditions occur.
	Force bool

	// Home is the location of cache
	Home string

	// Vendor contains the path to put the vendor packages
	Vendor string

	// Use a cache
	UseCache bool
	// Use Gopath to cache
	UseCacheGopath bool
	// Use Gopath as a source to read from
	UseGopath bool

	// UpdateVendored instructs the environment to update in a way that is friendly
	// to packages that have been "vendored in" (e.g. are copies of source, not repos)
	UpdateVendored bool

	// DeleteUnused deletes packages that are unused, but found in the vendor dir.
	DeleteUnused bool

	// ResolveAllFiles enables a resolver that will examine the dependencies
	// of every file of every package, rather than only following imported
	// packages.
	ResolveAllFiles bool
}

// VendorPath returns the path to the location to put vendor packages
func (i *Installer) VendorPath() string {
	if i.Vendor != "" {
		return i.Vendor
	}

	vp, err := gpath.Vendor()
	if err != nil {
		return filepath.FromSlash("./vendor")
	}

	return vp
}

// Install installs the dependencies from a Lockfile.
func (i *Installer) Install(lock *cfg.Lockfile, conf *cfg.Config) (*cfg.Config, error) {

	cwd, err := gpath.Vendor()
	if err != nil {
		return conf, err
	}

	// Create a config setup based on the Lockfile data to process with
	// existing commands.
	newConf := &cfg.Config{}
	newConf.Name = conf.Name

	newConf.Imports = make(cfg.Dependencies, len(lock.Imports))
	for k, v := range lock.Imports {
		newConf.Imports[k] = &cfg.Dependency{
			Name:        v.Name,
			Reference:   v.Version,
			Repository:  v.Repository,
			VcsType:     v.VcsType,
			Subpackages: v.Subpackages,
			Arch:        v.Arch,
			Os:          v.Os,
		}
	}

	newConf.DevImports = make(cfg.Dependencies, len(lock.DevImports))
	for k, v := range lock.DevImports {
		newConf.DevImports[k] = &cfg.Dependency{
			Name:        v.Name,
			Reference:   v.Version,
			Repository:  v.Repository,
			VcsType:     v.VcsType,
			Subpackages: v.Subpackages,
			Arch:        v.Arch,
			Os:          v.Os,
		}
	}

	newConf.DeDupe()

	if len(newConf.Imports) == 0 {
		msg.Info("No dependencies found. Nothing installed.\n")
		return newConf, nil
	}

	ConcurrentUpdate(newConf.Imports, cwd, i)
	ConcurrentUpdate(newConf.DevImports, cwd, i)
	return newConf, nil
}

// Checkout reads the config file and checks out all dependencies mentioned there.
//
// This is used when initializing an empty vendor directory, or when updating a
// vendor directory based on changed config.
func (i *Installer) Checkout(conf *cfg.Config, useDev bool) error {

	dest := i.VendorPath()

	if err := ConcurrentUpdate(conf.Imports, dest, i); err != nil {
		return err
	}

	if useDev {
		return ConcurrentUpdate(conf.DevImports, dest, i)
	}

	return nil
}

// Update updates all dependencies.
//
// It begins with the dependencies in the config file, but also resolves
// transitive dependencies. The returned lockfile has all of the dependencies
// listed, but the version reconciliation has not been done.
//
// In other words, all versions in the Lockfile will be empty.
func (i *Installer) Update(conf *cfg.Config) error {
	base := "."
	vpath := i.VendorPath()

	ic := newImportCache()

	m := &MissingPackageHandler{
		destination: vpath,

		cache:          i.UseCache,
		cacheGopath:    i.UseCacheGopath,
		useGopath:      i.UseGopath,
		home:           i.Home,
		force:          i.Force,
		updateVendored: i.UpdateVendored,
		Config:         conf,
		Use:            ic,
	}

	v := &VersionHandler{
		Destination: vpath,
		Use:         ic,
		Imported:    make(map[string]bool),
		Conflicts:   make(map[string]bool),
		Config:      conf,
	}

	// Update imports
	res, err := dependency.NewResolver(base)
	if err != nil {
		msg.Die("Failed to create a resolver: %s", err)
	}
	res.Config = conf
	res.Handler = m
	res.VersionHandler = v
	res.ResolveAllFiles = i.ResolveAllFiles
	msg.Info("Resolving imports")
	_, err = allPackages(conf.Imports, res)
	if err != nil {
		msg.Die("Failed to retrieve a list of dependencies: %s", err)
	}

	if len(conf.DevImports) > 0 {
		msg.Warn("dev imports not resolved.")
	}

	err = ConcurrentUpdate(conf.Imports, vpath, i)

	return err
}

func (i *Installer) List(conf *cfg.Config) []*cfg.Dependency {
	base := "."
	vpath := i.VendorPath()

	ic := newImportCache()

	v := &VersionHandler{
		Destination: vpath,
		Use:         ic,
		Imported:    make(map[string]bool),
		Conflicts:   make(map[string]bool),
		Config:      conf,
	}

	// Update imports
	res, err := dependency.NewResolver(base)
	if err != nil {
		msg.Die("Failed to create a resolver: %s", err)
	}
	res.Config = conf
	res.VersionHandler = v
	res.ResolveAllFiles = i.ResolveAllFiles

	msg.Info("Resolving imports")
	_, err = allPackages(conf.Imports, res)
	if err != nil {
		msg.Die("Failed to retrieve a list of dependencies: %s", err)
	}

	if len(conf.DevImports) > 0 {
		msg.Warn("dev imports not resolved.")
	}

	return conf.Imports
}

// ConcurrentUpdate takes a list of dependencies and updates in parallel.
func ConcurrentUpdate(deps []*cfg.Dependency, cwd string, i *Installer) error {
	done := make(chan struct{}, concurrentWorkers)
	in := make(chan *cfg.Dependency, concurrentWorkers)
	var wg sync.WaitGroup
	var lock sync.Mutex
	var returnErr error

	msg.Info("Downloading dependencies. Please wait...")

	for ii := 0; ii < concurrentWorkers; ii++ {
		go func(ch <-chan *cfg.Dependency) {
			for {
				select {
				case dep := <-ch:
					dest := filepath.Join(i.VendorPath(), dep.Name)
					if err := VcsUpdate(dep, dest, i.Home, i.UseCache, i.UseCacheGopath, i.UseGopath, i.Force, i.UpdateVendored); err != nil {
						msg.Warn("Update failed for %s: %s\n", dep.Name, err)
						// Capture the error while making sure the concurrent
						// operations don't step on each other.
						lock.Lock()
						if returnErr == nil {
							returnErr = err
						} else {
							returnErr = cli.NewMultiError(returnErr, err)
						}
						lock.Unlock()
					}
					wg.Done()
				case <-done:
					return
				}
			}
		}(in)
	}

	for _, dep := range deps {
		wg.Add(1)
		in <- dep
	}

	wg.Wait()

	// Close goroutines setting the version
	for ii := 0; ii < concurrentWorkers; ii++ {
		done <- struct{}{}
	}

	return returnErr
}

// allPackages gets a list of all packages required to satisfy the given deps.
func allPackages(deps []*cfg.Dependency, res *dependency.Resolver) ([]string, error) {
	if len(deps) == 0 {
		return []string{}, nil
	}

	vdir, err := gpath.Vendor()
	if err != nil {
		return []string{}, err
	}
	vdir += string(os.PathSeparator)
	ll, err := res.ResolveAll(deps)
	if err != nil {
		return []string{}, err
	}

	for i := 0; i < len(ll); i++ {
		ll[i] = strings.TrimPrefix(ll[i], vdir)
	}
	return ll, nil
}

// MissingPackageHandler is a dependency.MissingPackageHandler.
//
// When a package is not found, this attempts to resolve and fetch.
//
// When a package is found on the GOPATH, this notifies the user.
type MissingPackageHandler struct {
	destination                                          string
	home                                                 string
	cache, cacheGopath, useGopath, force, updateVendored bool
	Config                                               *cfg.Config
	Use                                                  *importCache
}

func (m *MissingPackageHandler) NotFound(pkg string) (bool, error) {
	root := util.GetRootFromPackage(pkg)

	// Skip any references to the root package.
	if root == m.Config.Name {
		return false, nil
	}

	dest := filepath.Join(m.destination, root)

	// This package may have been placed on the list to look for when it wasn't
	// downloaded but it has since been downloaded before coming to this entry.
	if _, err := os.Stat(dest); err == nil {
		msg.Debug("Found %s", dest)
		return true, nil
	}

	msg.Info("Fetching %s into %s", pkg, m.destination)

	d := m.Config.Imports.Get(root)
	// If the dependency is nil it means the Config doesn't yet know about it.
	if d == nil {
		d = m.Use.Get(root)
		// We don't know about this dependency so we create a basic instance.
		if d == nil {
			d = &cfg.Dependency{Name: root}
		}

		m.Config.Imports = append(m.Config.Imports, d)
	}
	if err := VcsGet(d, dest, m.home, m.cache, m.cacheGopath, m.useGopath); err != nil {
		return false, err
	}
	return true, nil
}

func (m *MissingPackageHandler) OnGopath(pkg string) (bool, error) {
	// If useGopath is false, we fall back to the strategy of fetching from
	// remote.
	if !m.useGopath {
		return m.NotFound(pkg)
	}

	root := util.GetRootFromPackage(pkg)

	// Skip any references to the root package.
	if root == m.Config.Name {
		return false, nil
	}

	msg.Info("Copying package %s from the GOPATH.", pkg)
	dest := filepath.Join(m.destination, pkg)
	// Find package on Gopath
	for _, gp := range gpath.Gopaths() {
		src := filepath.Join(gp, pkg)
		// FIXME: Should probably check if src is a dir or symlink.
		if _, err := os.Stat(src); err == nil {
			if err := os.MkdirAll(dest, os.ModeDir|0755); err != nil {
				return false, err
			}
			if err := gpath.CopyDir(src, dest); err != nil {
				return false, err
			}
			return true, nil
		}
	}

	msg.Error("Could not locate %s on the GOPATH, though it was found before.", pkg)
	return false, nil
}

// InVendor updates a package in the vendor/ directory to make sure the latest
// is available.
func (m *MissingPackageHandler) InVendor(pkg string) error {
	root := util.GetRootFromPackage(pkg)

	// Skip any references to the root package.
	if root == m.Config.Name {
		return nil
	}

	dest := filepath.Join(m.destination, root)

	d := m.Config.Imports.Get(root)
	// If the dependency is nil it means the Config doesn't yet know about it.
	if d == nil {
		d = m.Use.Get(root)
		// We don't know about this dependency so we create a basic instance.
		if d == nil {
			d = &cfg.Dependency{Name: root}
		}

		m.Config.Imports = append(m.Config.Imports, d)
	}

	if err := VcsUpdate(d, dest, m.home, m.cache, m.cacheGopath, m.useGopath, m.force, m.updateVendored); err != nil {
		return err
	}

	return nil
}

// VersionHandler handles setting the proper version in the VCS.
type VersionHandler struct {

	// If Try to use the version here if we have one. This is a cache and will
	// change over the course of setting versions.
	Use *importCache

	// Cache if importing scan has already occured here.
	Imported map[string]bool

	// Where the packages exist to set the version on.
	Destination string

	Config *cfg.Config

	// There's a problem where many sub-packages have been asked to set a version
	// and you can end up with numerous conflict messages that are exactly the
	// same. We are keeping track to only display them once.
	// the parent pac
	Conflicts map[string]bool
}

// Process imports dependencies for a package
func (d *VersionHandler) Process(pkg string) (e error) {
	root := util.GetRootFromPackage(pkg)

	// Skip any references to the root package.
	if root == d.Config.Name {
		return nil
	}

	// We have not tried to import, yet.
	// Should we look in places other than the root of the project?
	if d.Imported[root] == false {
		d.Imported[root] = true
		p := filepath.Join(d.Destination, root)
		f, deps, err := importer.Import(p)
		if f && err == nil {
			for _, dep := range deps {

				// The fist one wins. Would something smater than this be better?
				exists := d.Use.Get(dep.Name)
				if exists == nil && (dep.Reference != "" || dep.Repository != "") {
					d.Use.Add(dep.Name, dep)
				}
			}
		} else if err != nil {
			msg.Error("Unable to import from %s. Err: %s", root, err)
			e = err
		}
	}

	return
}

// SetVersion sets the version for a package. If that package version is already
// set it handles the case by:
// - keeping the already set version
// - proviting messaging about the version conflict
// TODO(mattfarina): The way version setting happens can be improved. Currently not optimal.
func (d *VersionHandler) SetVersion(pkg string) (e error) {
	root := util.GetRootFromPackage(pkg)

	// Skip any references to the root package.
	if root == d.Config.Name {
		return nil
	}

	v := d.Config.Imports.Get(root)

	dep := d.Use.Get(root)
	if dep != nil && v != nil {
		if v.Reference == "" && dep.Reference != "" {
			v.Reference = dep.Reference
			// Clear the pin, if set, so the new version can be used.
			v.Pin = ""
			dep = v
		} else if v.Reference != "" && dep.Reference != "" && v.Reference != dep.Reference {
			dest := filepath.Join(d.Destination, filepath.FromSlash(v.Name))
			dep = determineDependency(v, dep, dest)
		} else {
			dep = v
		}

	} else if v != nil {
		dep = v
	} else if dep != nil {
		// We've got an imported dependency to use and don't already have a
		// record of it. Append it to the Imports.
		d.Config.Imports = append(d.Config.Imports, dep)
	} else {
		// If we've gotten here we don't have any depenency objects.
		r, sp := util.NormalizeName(pkg)
		dep = &cfg.Dependency{
			Name: r,
		}
		if sp != "" {
			dep.Subpackages = []string{sp}
		}
		d.Config.Imports = append(d.Config.Imports, dep)
	}

	err := VcsVersion(dep, d.Destination)
	if err != nil {
		msg.Warn("Unable to set verion on %s to %s. Err: %s", root, dep.Reference, err)
		e = err
	}

	return
}

func determineDependency(v, dep *cfg.Dependency, dest string) *cfg.Dependency {
	repo, err := v.GetRepo(dest)
	if err != nil {
		singleWarn("Unable to access repo for %s\n", v.Name)
		singleInfo("Keeping %s %s", v.Name, v.Reference)
		return v
	}

	vIsRef := repo.IsReference(v.Reference)
	depIsRef := repo.IsReference(dep.Reference)

	// Both are references and they are different ones.
	if vIsRef && depIsRef {
		singleWarn("Conflict: %s ref is %s, but also asked for %s\n", v.Name, v.Reference, dep.Reference)
		singleInfo("Keeping %s %s", v.Name, v.Reference)
		return v
	} else if vIsRef {
		// The current one is a reference and the suggestion is a SemVer constraint.
		con, err := semver.NewConstraint(dep.Reference)
		if err != nil {
			singleWarn("Version issue for %s: '%s' is neither a reference or semantic version constraint\n", dep.Name, dep.Reference)
			singleInfo("Keeping %s %s", v.Name, v.Reference)
			return v
		}

		ver, err := semver.NewVersion(v.Reference)
		if err != nil {
			// The existing version is not a semantic version.
			singleWarn("Conflict: %s version is %s, but also asked for %s\n", v.Name, v.Reference, dep.Reference)
			singleInfo("Keeping %s %s", v.Name, v.Reference)
			return v
		}

		if con.Check(ver) {
			singleInfo("Keeping %s %s because it fits constraint '%s'", v.Name, v.Reference, dep.Reference)
			return v
		}
		singleWarn("Conflict: %s version is %s but does not meet constraint '%s'\n", v.Name, v.Reference, dep.Reference)
		singleInfo("Keeping %s %s", v.Name, v.Reference)
		return v
	} else if depIsRef {

		con, err := semver.NewConstraint(v.Reference)
		if err != nil {
			singleWarn("Version issue for %s: '%s' is neither a reference or semantic version constraint\n", v.Name, v.Reference)
			singleInfo("Keeping %s %s", v.Name, v.Reference)
			return v
		}

		ver, err := semver.NewVersion(dep.Reference)
		if err != nil {
			singleWarn("Conflict: %s version is %s, but also asked for %s\n", v.Name, v.Reference, dep.Reference)
			singleInfo("Keeping %s %s", v.Name, v.Reference)
			return v
		}

		if con.Check(ver) {
			v.Reference = dep.Reference
			singleInfo("Using %s %s because it fits constraint '%s'", v.Name, v.Reference, v.Reference)
			return v
		}
		singleWarn("Conflict: %s semantic version constraint is %s but '%s' does not meet the constraint\n", v.Name, v.Reference, v.Reference)
		singleInfo("Keeping %s %s", v.Name, v.Reference)
		return v
	}
	// Neither is a vcs reference and both could be semantic version
	// constraints that are different.

	_, err = semver.NewConstraint(dep.Reference)
	if err != nil {
		// dd.Reference is not a reference or a valid constraint.
		singleWarn("Version %s %s is not a reference or valid semantic version constraint\n", dep.Name, dep.Reference)
		singleInfo("Keeping %s %s", v.Name, v.Reference)
		return v
	}

	_, err = semver.NewConstraint(v.Reference)
	if err != nil {
		// existing.Reference is not a reference or a valid constraint.
		// We really should never end up here.
		singleWarn("Version %s %s is not a reference or valid semantic version constraint\n", v.Name, v.Reference)

		v.Reference = dep.Reference
		v.Pin = ""
		singleInfo("Using %s %s because it is a valid version", v.Name, v.Reference)
		return v
	}

	// Both versions are constraints. Try to merge them.
	// If either comparison has an || skip merging. That's complicated.
	ddor := strings.Index(dep.Reference, "||")
	eor := strings.Index(v.Reference, "||")
	if ddor == -1 && eor == -1 {
		// Add the comparisons together.
		newRef := v.Reference + ", " + dep.Reference
		v.Reference = newRef
		v.Pin = ""
		singleInfo("Combining %s semantic version constraints %s and %s", v.Name, v.Reference, dep.Reference)
		return v
	}
	singleWarn("Conflict: %s version is %s, but also asked for %s\n", v.Name, v.Reference, dep.Reference)
	singleInfo("Keeping %s %s", v.Name, v.Reference)
	return v
}

var warningMessage = make(map[string]bool)
var infoMessage = make(map[string]bool)

func singleWarn(ft string, v ...interface{}) {
	m := fmt.Sprintf(ft, v...)
	_, f := warningMessage[m]
	if !f {
		msg.Warn(m)
		warningMessage[m] = true
	}
}

func singleInfo(ft string, v ...interface{}) {
	m := fmt.Sprintf(ft, v...)
	_, f := infoMessage[m]
	if !f {
		msg.Info(m)
		infoMessage[m] = true
	}
}

type importCache struct {
	cache map[string]*cfg.Dependency
}

func newImportCache() *importCache {
	return &importCache{
		cache: make(map[string]*cfg.Dependency),
	}
}

func (i *importCache) Get(name string) *cfg.Dependency {
	d, f := i.cache[name]
	if f {
		return d
	}

	return nil
}

func (i *importCache) Add(name string, dep *cfg.Dependency) {
	i.cache[name] = dep
}
