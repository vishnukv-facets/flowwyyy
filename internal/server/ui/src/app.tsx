import { Route, Switch } from 'wouter'
import { Shell } from './components/Shell'
import { Overview } from './screens/Overview'
import { Analytics } from './screens/Analytics'
import { Sessions } from './screens/Sessions'
import { SessionDetail } from './screens/SessionDetail'
import { Tasks } from './screens/Tasks'
import { Owners } from './screens/Owners'
import { BrainGraph } from './screens/BrainGraph'
import { Projects } from './screens/Projects'
import { ProjectDetail } from './screens/ProjectDetail'
import { Playbooks } from './screens/Playbooks'
import { PlaybookDetail } from './screens/PlaybookDetail'
import { InboxScreen } from './screens/Inbox'
import { Attention } from './screens/Attention'
import { Chats } from './screens/Chats'
import { KnowledgeBase } from './screens/KB'
import { Memories } from './screens/Memories'
import { Workdirs } from './screens/Workdirs'
import { Trash } from './screens/Trash'
import { Settings } from './screens/Settings'
import { Connectors } from './screens/Connectors'
import { EmptyState } from './components/ui'
import { ClaudeFlowScene } from './components/ClaudeMascot'
import { FloatingTerminalsProvider } from './lib/floatingTerminals'

export function App() {
  return (
    <FloatingTerminalsProvider>
      <Shell>
        <Switch>
        <Route path="/" component={Overview} />
        <Route path="/analytics" component={Analytics} />
        <Route path="/sessions" component={Sessions} />
        <Route path="/session/:slug">{(p) => <SessionDetail slug={p.slug} />}</Route>
        <Route path="/task/:slug">{(p) => <SessionDetail slug={p.slug} />}</Route>
        <Route path="/tasks" component={Tasks} />
        <Route path="/owners" component={Owners} />
        <Route path="/graph" component={BrainGraph} />
        <Route path="/brain" component={BrainGraph} />
        <Route path="/projects" component={Projects} />
        <Route path="/project/:slug">{(p) => <ProjectDetail slug={p.slug} />}</Route>
        <Route path="/playbooks" component={Playbooks} />
        <Route path="/playbook/:slug">{(p) => <PlaybookDetail slug={p.slug} />}</Route>
        <Route path="/inbox" component={InboxScreen} />
        <Route path="/chats" component={Chats} />
        <Route path="/attention" component={Attention} />
        <Route path="/kb" component={KnowledgeBase} />
        <Route path="/memories" component={Memories} />
        <Route path="/workdirs" component={Workdirs} />
        <Route path="/connectors" component={Connectors} />
        <Route path="/settings" component={Settings} />
        <Route path="/trash" component={Trash} />
          <Route>
            <div className="page">
              <EmptyState icon={<ClaudeFlowScene />} title="Not found" hint="That route doesn't exist." />
            </div>
          </Route>
        </Switch>
      </Shell>
    </FloatingTerminalsProvider>
  )
}
