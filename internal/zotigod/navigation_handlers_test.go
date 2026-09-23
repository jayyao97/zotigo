package zotigod

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/jayyao97/zotigo/core/workspace"
)

func TestNavigationPublicRoutes(t *testing.T) {
	handler, _, _, ws := newCatalogSessionFixture(t)
	pin := fmt.Sprintf(`{"item":{"kind":"workspace","id":%q},"pinned":true}`, ws.ID)
	response := requestCatalog(t, handler, http.MethodPut, "/catalog/navigation/pin", pin)
	if response.Code != http.StatusOK {
		t.Fatalf("pin: %d %s", response.Code, response.Body)
	}
	order := fmt.Sprintf(`{"scope":"pinned","items":[{"kind":"workspace","id":%q}]}`, ws.ID)
	response = requestCatalog(t, handler, http.MethodPut, "/catalog/navigation/order", order)
	if response.Code != http.StatusOK {
		t.Fatalf("reorder: %d %s", response.Code, response.Body)
	}
	response = requestCatalog(t, handler, http.MethodGet, "/catalog/navigation", "")
	var state workspace.Navigation
	decodeCatalogData(t, response, &state)
	if !reflect.DeepEqual(state.Pinned, []workspace.NavigationItem{{Kind: "workspace", ID: ws.ID}}) {
		t.Fatalf("navigation = %+v", state)
	}
	for _, body := range []string{
		`{"scope":"pinned"}`,
		`{"scope":"invalid","items":[]}`,
		`{"scope":"pinned","items":[{"kind":"session","id":"missing"}]}`,
	} {
		response = requestCatalog(t, handler, http.MethodPut, "/catalog/navigation/order", body)
		if response.Code != http.StatusBadRequest && response.Code != http.StatusConflict {
			t.Fatalf("invalid order: %d %s", response.Code, response.Body)
		}
	}
	response = requestCatalog(t, handler, http.MethodPut, "/catalog/navigation/pin", fmt.Sprintf(`{"item":{"kind":"workspace","id":%q}}`, ws.ID))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing pinned: %d", response.Code)
	}
	response = requestCatalog(t, handler, http.MethodPost, "/catalog/navigation/import", `{"orders":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("import: %d %s", response.Code, response.Body)
	}
}
