import {render,screen,fireEvent,cleanup} from '@testing-library/react';
import {MemoryRouter} from 'react-router-dom';
import {it,expect,vi,afterEach} from 'vitest';
import {RegisterPage} from './RegisterPage';
import {authRequest} from '../api/auth';
vi.mock('../api/auth',()=>({authRequest:vi.fn()}));afterEach(()=>{cleanup();vi.resetAllMocks()});
it('registers a customer and carries generated identity to sign in',async()=>{
 vi.mocked(authRequest).mockResolvedValue({tenant:{id:'generated-tenant-id'}});
 render(<MemoryRouter><RegisterPage/></MemoryRouter>);
 fireEvent.change(screen.getByLabelText('Customer / company name'),{target:{value:'Example customer'}});
 fireEvent.change(screen.getByLabelText('Your email'),{target:{value:'owner@example.test'}});
 fireEvent.change(screen.getByLabelText('Password'),{target:{value:'strong-password'}});
 fireEvent.click(screen.getByRole('button',{name:'Register customer'}));
 expect(await screen.findByText('generated-tenant-id')).toBeTruthy();
 expect(screen.getByRole('link',{name:'Sign in to your workspace'})).toHaveAttribute('href','/login');
 expect(authRequest).toHaveBeenCalledWith('/v1/auth/register',expect.objectContaining({method:'POST'}));
});
