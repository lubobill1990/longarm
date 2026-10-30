import { skillsApi, type AgentSkills, type AgentSkillStatus } from '../api/local'
import { ErrorBox, useAction, usePoll } from '../components/common'
import { Section } from './ui'

/** The pill and the line under an agent's name. */
function describe(a: AgentSkillStatus): { pill: string; tone: string; help: string } {
  switch (a.state) {
    case 'current':
      return { pill: `installed ${a.installed_version ?? ''}`.trim(), tone: 'ok', help: a.dir }
    case 'outdated':
      return { pill: `${a.installed_version ?? 'older'} → ${a.bundled_version}`, tone: 'warn', help: a.dir }
    case 'modified':
      return {
        pill: 'changed by you',
        tone: 'warn',
        help: `Longarm leaves it alone: ${(a.modified ?? []).join(', ')} changed since it was installed. Remove or rename ${a.dir} to install the current skill.`,
      }
    case 'foreign':
      return { pill: 'not Longarm’s', tone: 'info', help: `${a.dir} holds a skill Longarm did not install; Longarm leaves it alone.` }
    default:
      return { pill: 'not installed', tone: '', help: a.dir || a.note || '' }
  }
}

/**
 * Settings > Agent skill: the Longarm skill that ships with this release,
 * installed into a coding agent's user-global skills directory only when
 * the user clicks Install; Remove deletes Longarm's own, unmodified files.
 */
export function AgentSkill() {
  const st = usePoll(skillsApi.list, 0)
  const { run, busy } = useAction()
  const act = (fn: () => Promise<AgentSkills>) => run(fn, (r) => r.message ?? 'done').then((r) => (r ? st.setData(r) : st.reload()))
  const s = st.data
  return (
    <Section card title="Agent skill" testId="agent-skill">
      <div className="field-help">
        Teaches Claude Code, Copilot CLI and other agents to use Longarm. Bundled with Longarm {s?.bundled_version ?? '…'}; installed copies are
        updated with Longarm unless you changed them.
      </div>
      <ErrorBox error={st.error} />
      <ul className="skill-list">
        {s?.agents.map((a) => {
          const d = describe(a)
          return (
            <li key={a.agent} className="skill-row" data-testid={`skill-${a.agent}`}>
              <div className="skill-what">
                <div>
                  <span className="field-label">{a.name}</span>{' '}
                  <span className={`pill ${a.detected ? 'ok' : ''}`} title={a.detected_by}>
                    {a.detected ? 'detected' : 'not found'}
                  </span>{' '}
                  <span className={`pill ${d.tone}`} data-testid={`skill-state-${a.agent}`}>
                    {d.pill}
                  </span>
                </div>
                <div className="field-help mono skill-dir" title={a.dir}>
                  {d.help}
                </div>
              </div>
              <div className="button-row">
                {a.state === 'absent' && a.dir && (
                  <button className={a.detected ? 'primary sm' : 'sm'} disabled={busy} onClick={() => act(() => skillsApi.install(a.agent))}>
                    Install
                  </button>
                )}
                {a.state === 'outdated' && (
                  <button className="primary sm" disabled={busy} onClick={() => act(() => skillsApi.install(a.agent))}>
                    Update
                  </button>
                )}
                {(a.state === 'current' || a.state === 'outdated') && (
                  <button className="sm" disabled={busy} onClick={() => act(() => skillsApi.remove(a.agent))}>
                    Remove
                  </button>
                )}
              </div>
            </li>
          )
        })}
      </ul>
    </Section>
  )
}
