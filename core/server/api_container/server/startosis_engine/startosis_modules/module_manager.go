package startosis_modules

// ModuleContentProvider A module manager allows you to get a Startosis module given a url
// It fetches the contents of the module for you
type ModuleContentProvider interface {
	GetModuleContentProvider(string) (string, error)
}
