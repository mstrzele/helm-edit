# Helm Edit

[![standard-readme compliant](https://img.shields.io/badge/standard--readme-OK-green.svg?style=flat-square)](https://github.com/RichardLitt/standard-readme)
[![CircleCI](https://img.shields.io/circleci/project/github/mstrzele/helm-edit.svg?style=flat-square)](https://circleci.com/gh/mstrzele/helm-edit)

> Edit a Helm release

This plugin adds `helm edit` command. It opens the editor defined by `HELM_EDITOR`, `KUBE_EDITOR` or `EDITOR` environment variable and allows to edit the values and upgrade a release.

## Install

```bash
$ helm plugin install https://github.com/nirroz93/helm-edit.git
```

## Usage

```bash
$ helm edit smiling-penguin
# Edits the smiling-penguin release
```

[![asciicast](https://asciinema.org/a/131663.png)](https://asciinema.org/a/131663)

## Maintainers

[@mstrzele](https://github.com/mstrzele)

## Contribute

PRs accepted.

Small note: If editing the README, please conform to the [standard-readme](https://github.com/RichardLitt/standard-readme) specification.

## License

[MIT](LICENSE) © 2017 Maciej Strzelecki
