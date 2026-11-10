// web/ is the admin page's Node project. This file makes it a module of its
// own so that ./... in the gateway module skips it: some npm packages ship Go
// sources (flatted does) that would otherwise be vetted, linted and tested.
module github.com/TheAsianFish/switchboard/web

go 1.27
