# Setup.
#export GOPATH="$CI_PROJECT_DIR/.cache/GOPATH:$GOPATH"
#export GOCACHE="$CI_PROJECT_DIR/.cache/GOCACHE"
#export GOLANGCI_LINT_CACHE=$CI_PROJECT_DIR/.cache/GOLANGCI

npm install

# Get changed files.
git remote add upstream https://codeberg.org/Curid/os-nvr.git
git fetch upstream dev
git diff --name-only upstream/dev "$CI_COMMIT_SHA"
files_changed="$(git diff --name-only upstream/dev $CI_COMMIT_SHA)"

if [ "$files_changed" == "" ]; then
	files_changed="$(git diff --name-only $CI_COMMIT_BEFORE_SHA $CI_COMMIT_SHA)" || echo "git error"
fi

./utils/ci.sh "$files_changed"
