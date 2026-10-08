import { useRoute } from '../router';
import { AuditScreen } from './AuditScreen';
import { BucketsScreen } from './BucketsScreen';
import { ClusterScreen } from './ClusterScreen';
import { GroupsScreen } from './GroupsScreen';
import { ObjectsScreen } from './ObjectsScreen';
import { PoliciesScreen } from './PoliciesScreen';
import { RolesScreen } from './RolesScreen';
import { UserScreen } from './UserScreen';
import { UsersScreen } from './UsersScreen';

/** Chooses the screen that belongs to the place in the address. */
export function Screens() {
  const route = useRoute();
  const [section = 'buckets', item] = route.segments;

  switch (section) {
    case 'buckets': {
      if (item === undefined) {
        return <BucketsScreen />;
      }
      const prefix = route.query.get('prefix') ?? '';
      // The key makes a fresh browser, back on its first page, for every prefix.
      return <ObjectsScreen key={`${item}\n${prefix}`} bucket={item} prefix={prefix} />;
    }
    case 'users':
      return item === undefined ? <UsersScreen /> : <UserScreen key={item} name={item} />;
    case 'groups':
      return <GroupsScreen />;
    case 'roles':
      return <RolesScreen />;
    case 'policies':
      return <PoliciesScreen />;
    case 'cluster':
      return <ClusterScreen />;
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
