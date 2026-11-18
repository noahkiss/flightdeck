import HabAPI
import SwiftUI

/// "Capcom jobs" in Capcom settings (capcom-service.md 9.6): a link to the list, shown only
/// when a hab in the app knows action words.
struct CapcomJobsSection: View {
    let center: RoundCenter

    var body: some View {
        if center.jobs.available {
            Section {
                NavigationLink {
                    CapcomJobsPage(center: center).navigationTitle("Capcom jobs")
                } label: {
                    LabeledContent("Capcom jobs", value: "\(center.jobs.jobs.count)")
                }
            } footer: {
                Text("Rounds in capcom mode. Choose what Capcom may do when each job's events come in.")
            }
        }
    }
}

/// The Capcom jobs list: each job's name, trigger and last outcome, and its switches.
struct CapcomJobsPage: View {
    let center: RoundCenter

    var body: some View {
        let jobs = center.jobs.jobs
        Form {
            if jobs.isEmpty {
                Text("No Capcom jobs. A round in capcom mode is a job.")
                    .foregroundStyle(.secondary)
            }
            ForEach(jobs) { job in
                Section {
                    NavigationLink {
                        RoundPage(center: center, habID: job.habID, roundID: job.round.id)
                    } label: {
                        CapcomJobSummary(job: job)
                    }
                    CapcomJobSwitches(jobs: center.jobs, job: job)
                } header: {
                    Text(job.round.name)
                }
            }
        }
    }
}

/// A job's trigger, hab, last outcome and pause mark.
struct CapcomJobSummary: View {
    let job: CapcomJob

    /// The trigger in plain words; an event trigger names its event.
    private var trigger: String {
        let t = job.round.trigger
        if t.kind == .event, let e = t.event, !e.isEmpty { return "on \(e)" }
        return RoundText.trigger(t)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text("\(trigger) · on \(job.habLabel)")
                .font(.callout)
            HStack(spacing: 6) {
                Text("Last: \(CapcomJobs.outcomeText(job.outcome))")
                if job.round.paused == true {
                    Text("Paused").foregroundStyle(.orange)
                }
            }
            .font(.caption)
            .foregroundStyle(.secondary)
        }
    }
}

/// One switch per action word. On asks the person to read the job's prompt first (inline,
/// below the switches); off takes the word away at once.
struct CapcomJobSwitches: View {
    let jobs: CapcomJobs
    let job: CapcomJob

    var body: some View {
        let busy = jobs.busy.contains(job.id)
        ForEach(CapcomJobs.words, id: \.word) { w in
            Toggle(w.title, isOn: Binding(
                get: { job.words.contains(w.word) },
                set: { on in Task { await jobs.set(job, w.word, on: on) } }))
                .disabled(busy)
        }
        if let ask = jobs.ask, ask.jobID == job.id {
            CapcomJobGrantView(jobs: jobs, ask: ask)
        }
        if let e = jobs.errors[job.id] {
            Text(e)
                .font(.callout)
                .foregroundStyle(.red)
        }
    }
}

/// The grant step: the job's prompt as the hab holds it, then Allow or Cancel. Allow sends
/// the revision this prompt was read at.
struct CapcomJobGrantView: View {
    let jobs: CapcomJobs
    let ask: CapcomJobGrant

    private var title: String {
        CapcomJobs.words.first { $0.word == ask.word }?.title ?? ask.word.rawValue
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Allow \"\(title)\"?")
                .font(.headline)
            if ask.changed {
                Label("The job changed. Read its prompt again.", systemImage: "exclamationmark.triangle")
                    .font(.callout)
                    .foregroundStyle(.orange)
            }
            Text("Capcom follows this prompt when the job's events come in:")
                .font(.callout)
                .foregroundStyle(.secondary)
            Text(ask.prompt.isEmpty ? "This job has no prompt." : ask.prompt)
                .font(.callout)
                .textSelection(.enabled)
                .fixedSize(horizontal: false, vertical: true)
                .padding(8)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(.background.secondary, in: RoundedRectangle(cornerRadius: 6))
            HStack {
                Button("Cancel", role: .cancel) { jobs.cancel() }
                Spacer()
                Button("Allow") { Task { await jobs.confirm() } }
                    .buttonStyle(.borderedProminent)
            }
            .buttonStyle(.bordered)
        }
        .padding(.vertical, 4)
    }
}
