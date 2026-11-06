package ownerapi

import "testing"

func TestValidateEngineSchedules(t *testing.T) {
	for _, input := range [][]EngineSchedule{nil, {{Engine: "bad"}}, {{Engine: "librespeed"}}, {{Engine: "ookla"}, {Engine: "ookla"}}, {{Engine: "ookla", Interval: 4}}, {{Engine: "ookla", Interval: 1441}}, {{Engine: "ookla", Offset: -1}}, {{Engine: "ookla", Offset: 1440}}} {
		if validateEngines(input) == nil {
			t.Fatal("accepted", input)
		}
	}
	input := []EngineSchedule{{Engine: "ookla", Interval: 5, Offset: 9}, {Engine: "librespeed"}}
	if err := validateEngines(input); err != nil || input[0].Offset != 4 || input[1].Interval != 30 {
		t.Fatal(input, err)
	}
}

func TestResultEngineFilters(t *testing.T) {
	for _, engine := range []string{"", "ookla", "librespeed"} {
		sql, args, ok := (ResultFilter{Engine: engine}).where()
		if !ok || sql == "" || engine != "" && (len(args) != 1 || args[0] != engine) {
			t.Fatal(sql, args, ok)
		}
	}
	if sql, args, ok := (ResultFilter{AllEngines: true}).where(); !ok || sql != "" || len(args) != 0 {
		t.Fatal(sql, args, ok)
	}
}
