import { createAction } from 'redux-actions';

import { addErrorToast } from './index';
import apiClient from '../api/Api';

export const processLoginRequest = createAction('PROCESS_LOGIN_REQUEST');
export const processLoginFailure = createAction('PROCESS_LOGIN_FAILURE');
export const processLoginSuccess = createAction('PROCESS_LOGIN_SUCCESS');

export const processLogin = values => async (dispatch) => {
    dispatch(processLoginRequest());
    try {
        await apiClient.login(values);
        const dashboardUrl = window.location.origin + window.location.pathname.replace('/login.html', '/');
        window.location.replace(dashboardUrl);
        dispatch(processLoginSuccess());
    } catch (error) {
        dispatch(addErrorToast({ error }));
        dispatch(processLoginFailure());
    }
};
