package guest

import (
	"errors"
	"testing"
)

func TestClassifyHostError(t *testing.T) {
	cases := []struct {
		name        string
		env         string
		wantReason  string // "" = no ausencia
		wantErrCode string // "" = sin error
		legacy      bool
		degradable  bool
		source      string
		sqlstate    string
	}{
		{
			name:       "nuevo: addon_not_installed",
			env:        `{"success":false,"error":{"code":"source_unavailable","legacy_code":"db_error","message":"ERROR: relation \"fiscal_documents\" does not exist (SQLSTATE 42P01)","details":{"source":"fiscal_documents","reason":"addon_not_installed","owners":["fiscal_mexico"],"installed_owners":[],"sqlstate":"42P01","degradable":true}}}`,
			wantReason: ReasonAddonNotInstalled, degradable: true, source: "fiscal_documents", sqlstate: "42P01",
		},
		{
			name:       "nuevo: addon_disabled",
			env:        `{"success":false,"error":{"code":"source_unavailable","details":{"source":"t","reason":"addon_disabled","degradable":true}}}`,
			wantReason: ReasonAddonDisabled, degradable: true, source: "t",
		},
		{
			name:       "nuevo: no degradable (dentro de tx) se respeta",
			env:        `{"success":false,"error":{"code":"source_unavailable","details":{"source":"t","reason":"table_missing","sqlstate":"42P01","degradable":false}}}`,
			wantReason: ReasonTableMissing, degradable: false, source: "t", sqlstate: "42P01",
		},
		{
			name:       "nuevo: schema_mismatch 42703",
			env:        `{"success":false,"error":{"code":"source_unavailable","details":{"source":"t","reason":"schema_mismatch","sqlstate":"42703","degradable":true}}}`,
			wantReason: ReasonSchemaMismatch, degradable: true, source: "t", sqlstate: "42703",
		},
		{
			name:       "nuevo sin razon: se deduce del SQLSTATE",
			env:        `{"success":false,"error":{"code":"source_unavailable","details":{"sqlstate":"42703"}}}`,
			wantReason: ReasonSchemaMismatch, sqlstate: "42703",
		},
		{
			name:       "viejo: db_error con 42P01",
			env:        `{"success":false,"error":{"code":"db_error","message":"ERROR: relation \"fiscal_documents\" does not exist (SQLSTATE 42P01)"}}`,
			wantReason: ReasonTableMissing, legacy: true, degradable: false, sqlstate: "42P01",
		},
		{
			name:       "viejo: db_error con 42703 es schema_mismatch, no table_missing",
			env:        `{"success":false,"error":{"code":"db_error","message":"ERROR: column \"x\" does not exist (SQLSTATE 42703)"}}`,
			wantReason: ReasonSchemaMismatch, legacy: true, degradable: false, sqlstate: "42703",
		},
		{
			name:        "viejo: 'does not exist' sin SQLSTATE NO es ausencia",
			env:         `{"success":false,"error":{"code":"db_error","message":"function foo() does not exist"}}`,
			wantErrCode: "db_error",
		},
		{
			name:        "viejo: otro SQLSTATE (23505) no es ausencia",
			env:         `{"success":false,"error":{"code":"db_error","message":"duplicate key (SQLSTATE 23505)"}}`,
			wantErrCode: "db_error",
		},
		{
			name:        "viejo: SQLSTATE truncado no es ausencia",
			env:         `{"success":false,"error":{"code":"db_error","message":"x (SQLSTATE 42P0)"}}`,
			wantErrCode: "db_error",
		},
		{
			name:        "42P01 en un code que no es db_error no se reinterpreta",
			env:         `{"success":false,"error":{"code":"forbidden","message":"SQLSTATE 42P01"}}`,
			wantErrCode: "forbidden",
		},
		{
			name:        "inyeccion: SQLSTATE en dato de usuario, sufijo real 22P02",
			env:         `{"success":false,"error":{"code":"db_error","message":"invalid input syntax for type uuid: \"SQLSTATE 42P01\" (SQLSTATE 22P02)"}}`,
			wantErrCode: "db_error",
		},
		{
			name:        "SQLSTATE a media frase sin sufijo (mensaje truncado) no se clasifica",
			env:         `{"success":false,"error":{"code":"db_error","message":"relation \"t\" does not exist SQLSTATE 42P01 y mas texto"}}`,
			wantErrCode: "db_error",
		},
		{
			name:        "sufijo sin parentesis no se clasifica",
			env:         `{"success":false,"error":{"code":"db_error","message":"x SQLSTATE 42P01"}}`,
			wantErrCode: "db_error",
		},
		{
			name:        "classified:true => db_error con 42P01 se queda como error",
			env:         `{"success":false,"error":{"code":"db_error","message":"relation \"t\" does not exist (SQLSTATE 42P01)","details":{"classified":true}}}`,
			wantErrCode: "db_error",
		},
		{
			name:       "classified:false => sigue el fallback",
			env:        `{"success":false,"error":{"code":"db_error","message":"relation \"t\" does not exist (SQLSTATE 42P01)","details":{"classified":false}}}`,
			wantReason: ReasonTableMissing, legacy: true, sqlstate: "42P01",
		},
		{
			name:       "sufijo con espacio final aun se acepta",
			env:        `{"success":false,"error":{"code":"db_error","message":"x (SQLSTATE 42P01)\n"}}`,
			wantReason: ReasonTableMissing, legacy: true, sqlstate: "42P01",
		},
		{name: "forbidden", env: `{"success":false,"error":{"code":"forbidden","message":"no db:read"}}`, wantErrCode: "forbidden"},
		{name: "invalid_request", env: `{"success":false,"error":{"code":"invalid_request","message":"x"}}`, wantErrCode: "invalid_request"},
		{name: "error sin code", env: `{"success":false,"error":{"message":"x"}}`, wantErrCode: "unknown"},
		{name: "fallo sin error", env: `{"success":false}`, wantErrCode: "unknown"},
		{name: "exito", env: `{"success":true,"data":{"rows":[]}}`},
		{
			name:       "exito con meta.absent (host con on_absent)",
			env:        `{"success":true,"data":{"rows":[]},"meta":{"absent":{"source":"fiscal_documents","reason":"addon_not_installed","owners":["fiscal_mexico"]}}}`,
			wantReason: ReasonAddonNotInstalled, degradable: true, source: "fiscal_documents",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := ClassifyHostError([]byte(tc.env))
			if tc.wantReason != "" {
				if out.Err != nil || out.Absent == nil {
					t.Fatalf("esperaba ausencia, obtuve %+v", out)
				}
				a := out.Absent
				if a.Reason != tc.wantReason || a.Legacy != tc.legacy || a.Degradable != tc.degradable ||
					a.Source != tc.source || a.SQLState != tc.sqlstate {
					t.Fatalf("ausencia = %+v", *a)
				}
				return
			}
			if out.Absent != nil {
				t.Fatalf("no debía ser ausencia: %+v", *out.Absent)
			}
			if tc.wantErrCode == "" {
				if out.Err != nil {
					t.Fatalf("esperaba éxito, obtuve %v", out.Err)
				}
				return
			}
			var he *HostError
			if !errors.As(out.Err, &he) || he.Code != tc.wantErrCode {
				t.Fatalf("err = %v, quería code %q", out.Err, tc.wantErrCode)
			}
		})
	}
}

func TestClassifyHostErrorMalformed(t *testing.T) {
	for _, env := range []string{"", "not json", "[1]"} {
		out := ClassifyHostError([]byte(env))
		if out.Err == nil || out.Absent != nil {
			t.Fatalf("%q: %+v", env, out)
		}
	}
}

func TestAbsencePreservesMessage(t *testing.T) {
	env := `{"success":false,"error":{"code":"source_unavailable","message":"orig (SQLSTATE 42P01)","details":{"reason":"table_missing"}}}`
	if got := ClassifyHostError([]byte(env)).Absent.Message; got != "orig (SQLSTATE 42P01)" {
		t.Fatalf("message = %q", got)
	}
}

func TestClassifyReadErrorLegacyDegradableOnlyForReads(t *testing.T) {
	env := []byte(`{"success":false,"error":{"code":"db_error","message":"x (SQLSTATE 42P01)"}}`)
	if a := ClassifyHostError(env).Absent; a == nil || a.Degradable {
		t.Fatalf("escritura/generico: forma vieja no debe ser degradable: %+v", a)
	}
	if a := ClassifyReadError(env).Absent; a == nil || !a.Degradable || !a.Legacy {
		t.Fatalf("lectura: %+v", a)
	}
	classified := []byte(`{"success":false,"error":{"code":"db_error","message":"x (SQLSTATE 42P01)","details":{"classified":true}}}`)
	if out := ClassifyReadError(classified); out.Absent != nil || out.Err == nil {
		t.Fatalf("classified en lectura: %+v", out)
	}
}
