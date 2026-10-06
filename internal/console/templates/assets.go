package templates

import "github.com/a-h/templ"

// assetURL is path with build as its v query, the URL shape whose response
// the console package's staticAsset marks immutable (#59).
func assetURL(path, build string) string {
	return path + "?v=" + build
}

// importMap points console.js's relative './keyboard.mjs' import at the
// versioned URL, so the module refetches with console.js (#59). build is
// hex from hashAssets, so it needs no escaping in the JSON.
func importMap(build string) templ.Component {
	return templ.Raw(`<script type="importmap">{"imports":{"/static/keyboard.mjs":"` +
		assetURL("/static/keyboard.mjs", build) + `"}}</script>`)
}
