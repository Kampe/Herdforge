# frozen_string_literal: true

# Hosted FAC-833 controls. Read the actual collector from YAML and execute it
# under its declared shell. Never execute a copied model of the production gate.
require 'fileutils'
require 'open3'
require 'tmpdir'
require 'yaml'

# Immutable pre-split inventory, available through the preserved full checkout.
# Intentional future setup/gate changes must revise this oracle in the same PR;
# additional control/upload pairs can extend controls_other without losing it.
BASE = '973ccd04a20c6cca0dec4c01c46ed8c2a76f344c'
WORKFLOW = '.github/workflows/ci.yml'
DEPENDENCIES = %w[foundation controls_other controls_receipt].freeze
RESULT_VARS = %w[FOUNDATION_RESULT OTHER_CONTROLS_RESULT RECEIPT_CONTROLS_RESULT].freeze
RECEIPT_STEP = 'Verify landed receipt guard controls'
CHECK_STEP = 'Verify CI dependency and collector controls'
UPLOAD_STEP = 'Upload CI dependency and collector control logs'
COLLECTOR_STEP = 'Require foundation and both control lanes'
RETENTION_TOOLS = 'Install landed retention tools'
RETENTION_STEP = 'Verify landed suite retention before foundation'
RETENTION_UPLOAD = 'Upload early landed suite retention logs'
RETENTION_STEPS = [RETENTION_TOOLS, RETENTION_STEP, RETENTION_UPLOAD].freeze
MODULE_STEP = 'Verify Gosec module invocation controls'
MODULE_UPLOAD = 'Upload Gosec module invocation logs'
MODULE_RUN = <<~'ZSH'
  set -euo pipefail
  report_parent=.verify-ci-parallel-logs
  mkdir -p -- "$report_parent"
  run_dir=$(mktemp -d "$report_parent/gosec-XXXXXX")
  FAC838_EVIDENCE_DIR="$PWD/$run_dir" go test -v -count=1 -timeout=120s ./pkg/security \
    -run '^TestSecurityGateModulesExactlyOnce$' >"$run_dir/go-test.log" 2>&1
  # A skipped or absent named test cannot satisfy this evidence gate.
  for phase in baseline duplicate-enumeration restored; do
    test -s "$run_dir/$phase/oracle.txt"
    test -s "$run_dir/$phase/scanner-calls.txt"
    test -s "$run_dir/$phase/result.txt"
  done
  test -s "$run_dir/cleanup/complete.txt"
ZSH

class ControlFailure < StandardError; end

def insist(condition, assertion)
  raise ControlFailure, assertion unless condition
end

def read_yaml(text)
  YAML.safe_load(text, aliases: false)
end

def check_job_graph(workflow, original)
  jobs = workflow.fetch('jobs')
  old_jobs = original.fetch('jobs')
  insist(jobs.keys.sort == (DEPENDENCIES + %w[gate fac151-hermetic coverage]).sort,
         'exact bounded job inventory')
  insist(workflow.reject { |key, _| key == 'jobs' } == original.reject { |key, _| key == 'jobs' },
         'trigger and per-ref cancellation preserved')
  %w[fac151-hermetic coverage].each do |job|
    insist(jobs[job] == old_jobs[job], "unchanged downstream job: #{job}")
  end
  DEPENDENCIES.each do |job|
    keys = %w[name runs-on steps]
    keys << 'needs' unless job == 'foundation'
    insist(jobs[job].keys.sort == keys.sort, "fixed job configuration: #{job}")
    insist(!jobs[job].key?('if') && !jobs[job].key?('continue-on-error') &&
           !jobs[job].key?('strategy'), "unconditional fixed job: #{job}")
    expected = job == 'foundation' ? [] : ['foundation']
    insist(Array(jobs[job]['needs']) == expected, "dependency edge: #{job}")
    insist(jobs[job]['runs-on'] == 'ubuntu-latest', "runner preserved: #{job}")
  end
end

def check_setup(jobs, old_steps, make_index, setup, all_steps)
  DEPENDENCIES.each do |job|
    steps = jobs[job].fetch('steps')
    # Only foundation inserts this explicitly checked fast-failure group.
    # The original setup objects and order remain mandatory in every lane.
    steps = steps.reject { |step| RETENTION_STEPS.include?(step['name']) } if job == 'foundation'
    insist(steps.take(setup.length) == setup, "pinned setup preserved: #{job}")
  end
  make_steps = all_steps.select { |step| step['run'] == 'make lint test-unit test-race preflight' }
  insist(make_steps == [old_steps[make_index]], 'foundation make command exactly once')
  insist(jobs['foundation']['steps'].include?(old_steps[make_index]), 'make belongs to foundation')
end

def check_original_controls(controls, all_steps, lane_steps)
  receipt_index = controls.index { |step| step['name'] == RECEIPT_STEP }
  insist(!receipt_index.nil?, 'original receipt anchor exists')
  expected_receipt = controls.slice(receipt_index, 2)
  expected_other = controls - expected_receipt
  # Preserve every old step object (including commands, timeout and artifact
  # policy). New controls may be appended, but cannot replace the old inventory.
  controls.each do |step|
    matches = all_steps.select { |candidate| candidate['name'] == step['name'] }
    insist(matches == [step], "original control step preserved exactly once: #{step['name']}")
  end
  %w[controls_other controls_receipt].zip([expected_other, expected_receipt]).each do |job, expected|
    names = expected.map { |step| step['name'] }
    insist(lane_steps[job].select { |step| names.include?(step['name']) } == expected,
           "control order and placement: #{job}")
  end
  insist(lane_steps['controls_receipt'] == expected_receipt, 'receipt lane stays isolated')
end

def check_lane_controls(lane_steps)
  drivers = []
  artifacts = []
  lane_steps.each do |job, steps|
    insist(steps.length.even?, "paired control and upload: #{job}")
    steps.each_slice(2) do |driver, upload|
      insist(driver.key?('run') && !driver.key?('if') && !driver.key?('continue-on-error'),
             "required driver: #{driver['name']}")
      insist(driver['timeout-minutes'].is_a?(Integer) && driver['timeout-minutes'].between?(1, 45),
             "bounded driver: #{driver['name']}")
      scripts = driver['run'].scan(%r{scripts/verify-[a-z0-9-]+\.zsh})
      insist(!scripts.empty? && scripts.all? { |script| File.file?(script) },
             "existing control scripts: #{driver['name']}")
      drivers.concat(scripts)
      settings = upload.fetch('with')
      insist(upload['uses'] == 'actions/upload-artifact@v4' && upload['if'] == 'always()' &&
             !upload.key?('continue-on-error') && settings['include-hidden-files'] == true &&
             settings['if-no-files-found'] == 'error' && settings['retention-days'] == 7 &&
             settings['path'].is_a?(String) && settings['path'].start_with?('.verify-'),
             "durable failure upload: #{upload['name']}")
      artifacts << settings.fetch('name')
    end
  end
  insist(drivers.uniq == drivers, 'each control script invoked exactly once')
  insist(artifacts.uniq == artifacts, 'each control artifact uploaded exactly once')
end

def check_foundation(jobs, setup, make_step)
  foundation_steps = jobs['foundation']['steps']
  tools = foundation_steps.select { |step| step['name'] == RETENTION_TOOLS }
  insist(tools == [{ 'name' => RETENTION_TOOLS,
                    'run' => 'sudo apt-get install -y --no-install-recommends jq coreutils' }],
         'early suite retention tools are required')
  retention = foundation_steps.select { |step| step['name'] == RETENTION_STEP }
  invocation = "timeout -k 2s 20s zsh scripts/verify-landed-suite-retention.zsh \\\n    scripts/lib/landed-control-suites.zsh \"$run_dir/suite-retention\""
  insist(retention.length == 1 && retention[0].keys.sort == %w[name run shell timeout-minutes] &&
         retention[0]['shell'] == 'zsh {0}' && retention[0]['timeout-minutes'] == 1 &&
         retention[0]['run'].include?(invocation), 'early suite retention fixture is required')
  retention_upload = foundation_steps.select { |step| step['name'] == RETENTION_UPLOAD }
  insist(retention_upload == [{ 'name' => RETENTION_UPLOAD, 'if' => 'always()',
                               'uses' => 'actions/upload-artifact@v4',
                               'with' => { 'name' => 'verify-landed-suite-retention-logs',
                                           'path' => '.verify-landed-receipt-logs/foundation-*',
                                           'include-hidden-files' => true, 'if-no-files-found' => 'error',
                                           'retention-days' => 7 } }],
         'early suite retention evidence survives failures')
  verifier = foundation_steps.select { |step| step['name'] == CHECK_STEP }
  insist(verifier.length == 1 && verifier[0]['run'] == 'ruby scripts/verify-ci-parallel.rb' &&
         verifier[0]['timeout-minutes'] == 3 && !verifier[0].key?('if') &&
         !verifier[0].key?('continue-on-error'), 'structural controls are required')
  upload = foundation_steps.select { |step| step['name'] == UPLOAD_STEP }
  insist(upload.length == 1 && upload[0]['if'] == 'always()' &&
         upload[0]['uses'] == 'actions/upload-artifact@v4' && !upload[0].key?('continue-on-error') &&
         upload[0]['with'] == { 'name' => 'verify-ci-parallel-logs', 'path' => '.verify-ci-parallel-logs/run-*',
                               'include-hidden-files' => true, 'if-no-files-found' => 'error', 'retention-days' => 7 },
         'structural evidence survives failures')
  parser = { 'name' => 'Install CI fixture parser',
             'run' => 'sudo apt-get install -y --no-install-recommends ruby' }
  modules = foundation_steps.select { |step| step['name'] == MODULE_STEP }
  insist(modules == [{ 'name' => MODULE_STEP, 'shell' => 'zsh {0}', 'timeout-minutes' => 5,
                      'run' => MODULE_RUN }], 'module invocation controls and completion evidence are required')
  module_upload = foundation_steps.select { |step| step['name'] == MODULE_UPLOAD }
  insist(module_upload == [{ 'name' => MODULE_UPLOAD, 'if' => 'always()',
                            'uses' => 'actions/upload-artifact@v4',
                            'with' => { 'name' => 'verify-gosec-modules-logs',
                                        'path' => '.verify-ci-parallel-logs/gosec-*',
                                        'include-hidden-files' => true, 'if-no-files-found' => 'error',
                                        'retention-days' => 7 } }],
         'module invocation evidence survives failures')
  early_index = setup.index { |step| step['name'] == 'Install zsh' }
  insist(!early_index.nil?, 'original shell setup anchor exists')
  foundation_setup = setup.dup.insert(early_index + 1, tools[0], retention[0], retention_upload[0])
  insist(foundation_steps == foundation_setup + [parser, verifier[0], upload[0], modules[0], module_upload[0], make_step],
         'foundation runs only setup, orchestration controls and original make')
end

def check_collector(jobs, setup)
  gate = jobs.fetch('gate')
  insist(gate.keys.sort == %w[if name needs runs-on steps timeout-minutes].sort &&
         gate['runs-on'] == 'ubuntu-latest', 'fixed collector configuration')
  insist(gate['name'] == 'Build, Preflight & Test Suite', 'required check name preserved')
  insist(gate['needs'] == DEPENDENCIES, 'collector dependency edges')
  insist(gate['if'] == '${{ always() }}' && !gate.key?('continue-on-error') && !gate.key?('strategy'),
         'collector runs after unsuccessful dependencies')
  insist(gate['timeout-minutes'] == 5, 'collector outer bound')
  insist(gate['steps'].length == 2 && gate['steps'][0] == setup.find { |step| step['name'] == 'Install zsh' },
         'collector shell prerequisite is required')
  collector = gate['steps'][1]
  expected_env = RESULT_VARS.zip(DEPENDENCIES.map { |job| "${{ needs.#{job}.result }}" }).to_h
  insist(collector['name'] == COLLECTOR_STEP && collector['shell'] == 'zsh {0}' &&
         collector['env'] == expected_env && !collector.key?('if') &&
         !collector.key?('continue-on-error'), 'collector consumes exact dependency results')
  collector.fetch('run')
end

def check_structure(workflow, original)
  check_job_graph(workflow, original)
  jobs = workflow.fetch('jobs')
  old_steps = original.fetch('jobs').fetch('gate').fetch('steps')
  make_index = old_steps.index { |step| step['run'] == 'make lint test-unit test-race preflight' }
  insist(!make_index.nil?, 'original foundation anchor exists')
  setup = old_steps.take(make_index)
  all_steps = jobs.values.flat_map { |job| job.fetch('steps') }
  check_setup(jobs, old_steps, make_index, setup, all_steps)
  lane_steps = %w[controls_other controls_receipt].to_h do |job|
    [job, jobs[job].fetch('steps').drop(setup.length)]
  end
  check_original_controls(old_steps.drop(make_index + 1), all_steps, lane_steps)
  check_lane_controls(lane_steps)
  check_foundation(jobs, setup, old_steps[make_index])
  check_collector(jobs, setup)
end

def run_shell(script, env, label, directory, syntax: false)
  argv = ['timeout', '--signal=KILL', '5s', 'zsh']
  argv << '-n' if syntax
  stdout, stderr, result = Open3.capture3(env, *argv, script)
  File.write(File.join(directory, "#{label}.log"), "exit=#{result.exitstatus}\n#{stdout}#{stderr}")
  [result, stderr]
end

def exercise_collector(script, label, directory)
  success = RESULT_VARS.to_h { |variable| [variable, 'success'] }
  result, = run_shell(script, success, "#{label}-success", directory)
  insist(result.success?, "collector rejects all-success: #{label}")
  RESULT_VARS.zip(DEPENDENCIES).each do |variable, job|
    ['failure', 'cancelled', 'skipped', '', nil, 'neutral', 'Success'].each_with_index do |value, index|
      result, stderr = run_shell(script, success.merge(variable => value), "#{label}-#{job}-#{index}", directory)
      diagnostic = "Required CI job did not succeed: #{job}:#{value}\n"
      insist(stderr == diagnostic, "wrong refusal diagnostic: #{job}:#{value}")
      insist(result.exitstatus == 1, "collector accepted #{job}:#{value}")
    end
  end
end

def structure_mutant(workflow, original, label, assertion, summary)
  mutant = Marshal.load(Marshal.dump(workflow))
  yield mutant
  begin
    check_structure(mutant, original)
  rescue ControlFailure => error
    insist(error.message == assertion, "WRONG-ASSERTION #{label}: #{error.message}")
    summary.puts("KILLED #{label}: #{assertion}")
    return
  end
  raise ControlFailure, "SURVIVED #{label}"
end

FileUtils.mkdir_p('.verify-ci-parallel-logs')
allocated = Dir.mktmpdir('run-', '.verify-ci-parallel-logs')
directory = File.join('.verify-ci-parallel-logs', File.basename(allocated))
File.open(File.join(directory, 'summary.log'), 'w') do |summary|
  summary.sync = true
  begin
    original_text, stderr, result = Open3.capture3('git', 'show', "#{BASE}:#{WORKFLOW}")
    insist(result.success?, "cannot read immutable original inventory: #{stderr}")
    original = read_yaml(original_text)
    workflow_text = File.read(WORKFLOW)
    workflow = read_yaml(workflow_text)
    script = check_structure(workflow, original)
    summary.puts('PASS original step inventory, setup, dependency graph, Docker and coverage')
    structure_mutant(workflow, original, 'missing-dependency', 'collector dependency edges', summary) do |mutant|
      mutant['jobs']['gate']['needs'].delete('controls_receipt')
    end
    structure_mutant(workflow, original, 'missing-retention-tools',
                     'early suite retention tools are required', summary) do |mutant|
      mutant['jobs']['foundation']['steps'].reject! { |step| step['name'] == RETENTION_TOOLS }
    end
    structure_mutant(workflow, original, 'missing-retention-preflight',
                     'early suite retention fixture is required', summary) do |mutant|
      mutant['jobs']['foundation']['steps'].reject! { |step| step['name'] == RETENTION_STEP }
    end
    structure_mutant(workflow, original, 'lost-retention-artifact',
                     'early suite retention evidence survives failures', summary) do |mutant|
      step = mutant['jobs']['foundation']['steps'].find { |entry| entry['name'] == RETENTION_UPLOAD }
      step.delete('if')
    end
    structure_mutant(workflow, original, 'missing-module-controls',
                     'module invocation controls and completion evidence are required', summary) do |mutant|
      mutant['jobs']['foundation']['steps'].reject! { |step| step['name'] == MODULE_STEP }
    end
    structure_mutant(workflow, original, 'missing-module-completion-evidence',
                     'module invocation controls and completion evidence are required', summary) do |mutant|
      step = mutant['jobs']['foundation']['steps'].find { |entry| entry['name'] == MODULE_STEP }
      step['run'] = step['run'].sub('test -s "$run_dir/cleanup/complete.txt"', ':')
    end
    structure_mutant(workflow, original, 'lost-module-artifact',
                     'module invocation evidence survives failures', summary) do |mutant|
      step = mutant['jobs']['foundation']['steps'].find { |entry| entry['name'] == MODULE_UPLOAD }
      step.delete('if')
    end
    old_driver = original['jobs']['gate']['steps'].find { |step| step['name'] == RECEIPT_STEP }
    structure_mutant(workflow, original, 'missing-driver',
                     "original control step preserved exactly once: #{RECEIPT_STEP}", summary) do |mutant|
      mutant['jobs']['controls_receipt']['steps'].delete(old_driver)
    end
    artifact_name = 'Upload landed receipt control logs'
    structure_mutant(workflow, original, 'lost-failure-artifact',
                     "original control step preserved exactly once: #{artifact_name}", summary) do |mutant|
      step = mutant['jobs']['controls_receipt']['steps'].find { |entry| entry['name'] == artifact_name }
      step.delete('if')
    end

    script_path = File.join(directory, 'collector.zsh')
    File.write(script_path, script)
    syntax, = run_shell(script_path, {}, 'baseline-syntax', directory, syntax: true)
    insist(syntax.success?, 'BROKEN-RUN baseline collector syntax')
    exercise_collector(script_path, 'baseline', directory)
    summary.puts('PASS all-success and 21 failure/cancel/skip/empty/missing/unexpected result cases')

    anchor = 'exit 1 # FAC-833 refusal'
    insist(script.scan(anchor).length == 1, 'exact refusal mutation anchor')
    mutant_path = File.join(directory, 'collector-refusal-disabled.zsh')
    File.write(mutant_path, script.sub(anchor, ': # FAC-833 refusal disabled'))
    syntax, = run_shell(mutant_path, {}, 'mutant-syntax', directory, syntax: true)
    insist(syntax.success?, 'BROKEN-RUN refusal-disabled collector syntax')
    begin
      exercise_collector(mutant_path, 'mutant', directory)
      raise ControlFailure, 'SURVIVED refusal-disabled collector'
    rescue ControlFailure => error
      insist(error.message == 'collector accepted foundation:failure',
             "WRONG-ASSERTION refusal-disabled collector: #{error.message}")
      summary.puts("KILLED refusal-disabled collector: #{error.message}")
    end
    insist(File.read(WORKFLOW) == workflow_text && File.read(script_path) == script,
           'production workflow and baseline collector remain byte-identical')
    exercise_collector(script_path, 'restored', directory)
    summary.puts('PASS restored collector; production workflow unchanged')
  rescue StandardError => error
    summary.puts("FAIL #{error.class}: #{error.message}")
    warn "FAC-833 controls failed: #{error.message}"
    exit 1
  end
end
puts "FAC-833 controls passed; evidence: #{directory}"
