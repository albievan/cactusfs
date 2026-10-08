# Swagger UI assets

The files in this directory are taken unchanged from the npm package
`swagger-ui-dist` 5.33.1 (Apache-2.0; see `LICENSE` and `NOTICE`), tarball
integrity
`sha512-H872wWkA53bFIsGgi7OWgmq+CRWw3nFQGdJWRqOB9wNwTm6e5ol34+qPDkV4AJlK+gglM2EsJiOOzsGsEGbluA==`.
They are committed so that the binary builds from a fresh clone with Go alone
and serves the documentation without fetching anything.

`index.html` and `initializer.js` next to this directory are ours.

To update: download the new tarball from the npm registry, verify its
integrity, replace `swagger-ui-bundle.js`, `swagger-ui-bundle.js.LICENSE.txt`,
`swagger-ui.css`, `favicon-32x32.png`, `LICENSE` and `NOTICE`, and change the
version here and in `PLAN.md`.
