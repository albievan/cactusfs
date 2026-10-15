import { useRoute } from '../router';
import { AuditScreen } from './AuditScreen';
import { BucketsScreen } from './BucketsScreen';
import { ClusterScreen } from './ClusterScreen';
import { GroupsScreen } from './GroupsScreen';
import { ObjectsScreen } from './ObjectsScreen';
import { PoliciesScreen } from './PoliciesScreen';
import { RoleScreen } from './RoleScreen';
import { RolesScreen } from './RolesScreen';
import { SearchScreen } from './SearchScreen';
import { SettingsScreen } from './SettingsScreen';
import { UserScreen } from './UserScreen';
import { UsersScreen } from './UsersScreen';

interface ScreensProps {
  /**
   * Whether the session was signed in to with a password, as the API says:
   * only then is a password asked for again before every object of a bucket
   * is deleted. The API decides; this only chooses what is offered.
   */
  passwordSession?: boolean;
}

/** Chooses the screen that belongs to the place in the address. */
export function Screens({ passwordSession = false }: ScreensProps) {
  const route = useRoute();
  const [section = 'buckets', item] = route.segments;

  switch (section) {
    case 'buckets': {
      if (item === undefined) {
        return <BucketsScreen />;
      }
      const prefix = route.query.get('prefix') ?? '';
      // The key makes a fresh browser, back on its first page, for every prefix.
      return (
        <ObjectsScreen key={`${item}\n${prefix}`} bucket={item} prefix={prefix} passwordSession={passwordSession} />
      );
    }
    case 'search': {
      const query = route.query.get('q') ?? '';
      // The key makes a fresh search, back on its first page, for every query.
      return <SearchScreen key={query} query={query} />;
    }
    case 'users':
      return item === undefined ? <UsersScreen /> : <UserScreen key={item} name={item} />;
    case 'groups':
      return <GroupsScreen />;
    case 'roles':
      return item === undefined ? <RolesScreen /> : <RoleScreen key={item} name={item} />;
    case 'policies':
      return <PoliciesScreen />;
    case 'cluster':
      return <ClusterScreen />;
    case 'settings':
      return <SettingsScreen />;
    case 'audit':
      return <AuditScreen after={parseAfter(route.query.get('after'))} />;
    default:
      return (
        <>
          <h1>Not found</h1>
          <p>There is no such screen in the console.</p>
        </>
      );
  }
}

function parseAfter(value: string | null): number {
  const after = Number.parseInt(value ?? '', 10);
  return Number.isSafeInteger(after) && after > 0 ? after : 0;
}
