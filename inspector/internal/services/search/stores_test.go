package search

import (
	"context"
	"errors"
	"testing"

	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/magento"
)

type fakeReader struct {
	magento.Reader
	env *magento.Env
	err error
}

func (f fakeReader) Load(context.Context) (*magento.Env, error) {
	return f.env, f.err
}

func TestDBCredentials(t *testing.T) {
	user, pass, db := dbCredentials([]string{"MYSQL_ROOT_PASSWORD=root", "MYSQL_DATABASE=magento", "MYSQL_USER=magento", "MYSQL_PASSWORD=secret=x", "PATH=/bin"})
	if user != "magento" || pass != "secret=x" || db != "magento" {
		t.Errorf("got %q %q %q", user, pass, db)
	}
	user, _, db = dbCredentials([]string{"MARIADB_USER=m", "MARIADB_DATABASE=d"})
	if user != "m" || db != "d" {
		t.Errorf("mariadb names: %q %q", user, db)
	}
}

func TestStoreLoader(t *testing.T) {
	ctx := context.Background()
	d := &docker.Fake{}
	cases := []struct {
		name    string
		reader  magento.Reader
		magento bool
		err     bool
	}{
		{"no reader", nil, false, false},
		{"generic", fakeReader{env: &magento.Env{Generic: true}}, false, false},
		{"load fails", fakeReader{err: errors.New("php-fpm down")}, true, true},
		{"no db", fakeReader{env: &magento.Env{DBName: "magento"}}, true, true},
		{"bad prefix", fakeReader{env: &magento.Env{TablePrefix: "x`; DROP"}}, true, true},
	}
	for _, c := range cases {
		st := (&storeLoader{docker: d, magento: c.reader}).load(ctx)
		if st.Magento != c.magento || (st.Err != nil) != c.err || st.Stores != nil {
			t.Errorf("%s: %+v", c.name, st)
		}
	}
}
