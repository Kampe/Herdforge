# Shared by both landed-control drivers. The caller supplies suites, sep,
# run_dir and its existing run_focused/assert_required_identities/note helpers.
# Loading this file performs no work and launches no child processes.

run_all_suites() {
	local label=$1 record fields pkg selector required suite_exit slug stem
	local -i suite_index=0
	for record in "${suites[@]}"; do
		(( suite_index += 1 ))
		fields=("${(@ps:$sep:)record}")
		pkg=$fields[1]; selector=$fields[2]; required=$fields[3]
		slug=${${pkg//.\//}//\//-}
		slug=${slug%-}
		stem="$label-s${suite_index}-$slug"
		# A package can occur in several independently checked rows. Keep the
		# ordinal in BOTH raw paths, and record their meaning before execution
		# so failure artifacts can be attributed without reconstructing a run.
		if ! jq -cn --arg phase "$label" --argjson ordinal "$suite_index" \
			--arg package "$pkg" --arg selector "$selector" --arg required "$required" \
			--arg events "$stem.json" --arg stderr "$stem.err" \
			'{phase:$phase, ordinal:$ordinal, package:$package, selector:$selector,
			  required:$required, events:$events, stderr:$stderr}' >> "$run_dir/suites.jsonl"; then
			note "$label suite $suite_index: could not record the suite identity"
			return 1
		fi
		note "$label suite $suite_index $pkg selector=$selector events=$stem.json stderr=$stem.err"
		suite_exit=$(run_focused "$pkg" "$selector" "$run_dir/$stem.json" "$run_dir/$stem.err")
		if (( suite_exit != 0 )); then
			note "$label suite $suite_index $pkg FAILED (exit $suite_exit) - the suite must pass before any mutant means anything"
			[[ -s "$run_dir/$stem.err" ]] && tail -n 20 -- "$run_dir/$stem.err" >&2
			return 1
		fi
		assert_required_identities "$run_dir/$stem.json" "$label suite $suite_index $pkg" ${=required} || return 1
	done
	return 0
}
